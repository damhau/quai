package siem_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/audit/siem"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/splunktest"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var ctx = context.Background()

// The personal values the events hold: the export carries their pseudonyms only.
const (
	person = "alice.example"
	reason = "Zürich ☃ 🚀 rollout"
)

// syncBuffer is a log the exporter writes to while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	t       *testing.T
	store   *store.Store
	url     string
	ca      authority
	vars    map[string]string
	secrets *secrets.Store
	log     *syncBuffer
}

func setup(t *testing.T) *env {
	t.Helper()
	u := pgtest.URL(t)
	s, err := store.Open(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	e := &env{t: t, store: s, url: u, ca: newAuthority(t), vars: map[string]string{}, log: &syncBuffer{}}
	e.secrets = secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := e.vars[n]; return v, ok }}})
	return e
}

// network trusts the certificates in PEM, through network.caFile as an installation would.
func (e *env) network(pems ...[]byte) *netx.Network {
	e.t.Helper()
	file := filepath.Join(e.t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, bytes.Join(pems, []byte("\n")), 0o600); err != nil {
		e.t.Fatal(err)
	}
	n, err := netx.New(file)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) exporter(network *netx.Network, dests ...config.AuditExport) *siem.Exporter {
	e.t.Helper()
	x, err := siem.New(siem.Options{Store: e.store, Destinations: dests, Secrets: e.secrets, Network: network,
		Log: slog.New(slog.NewTextHandler(e.log, nil)), Hostname: "worker-1", Version: "test"})
	if err != nil {
		e.t.Fatal(err)
	}
	return x
}

// record writes n events, as a person, with personal data and characters beyond ASCII.
func (e *env) record(n int) {
	e.t.Helper()
	actx := audit.WithActor(ctx, audit.Actor{Type: audit.User, ID: person, IP: "192.0.2.7", AuthTime: time.Now()})
	for i := range n {
		if err := e.store.Tx(actx, func(tx pgx.Tx, _ *db.Queries) error {
			return audit.Add(actx, tx, audit.Event{Action: "request.submitted", Object: audit.Object{Type: "request", ID: "r-1", Resource: "infra/vm/zürich"},
				Details: map[string]any{"n": i, "reason": audit.Personal(reason), "note": "émoji 🚀"}})
		}); err != nil {
			e.t.Fatal(err)
		}
	}
}

// events returns the log's events.
func (e *env) events() []audit.Record {
	e.t.Helper()
	rows, err := e.store.AuditEventsFrom(ctx, db.AuditEventsFromParams{Seq: 1, MaxRows: 100000})
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]audit.Record, len(rows))
	for i, r := range rows {
		out[i] = audit.RecordOf(r)
	}
	return out
}

// cursor returns the last seq a destination took, 0 for none.
func (e *env) cursor(name string) int64 {
	e.t.Helper()
	c, err := e.store.AuditExportCursor(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	} else if err != nil {
		e.t.Fatal(err)
	}
	return c.Seq
}

// delivered checks what a destination received, each record's JSON as it came: every event of
// the log, in order, the same as the log's, with no gap (repeats allowed), personal data as
// pseudonyms; and that the chain verifies from what the SIEM holds.
func (e *env) delivered(raw [][]byte) {
	e.t.Helper()
	want := e.events()
	v := audit.NewVerifier(audit.Keys{})
	var last int64
	for i, b := range raw {
		if bytes.Contains(b, []byte(person)) || bytes.Contains(b, []byte("192.0.2.7")) || bytes.Contains(b, []byte("rollout")) {
			e.t.Errorf("record %d carries a clear personal value: %s", i, b)
		}
		var r audit.Record
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			e.t.Fatalf("record %d: %v: %s", i, err, b)
		}
		if r.Seq < 1 || r.Seq > int64(len(want)) {
			e.t.Fatalf("record %d: seq %d is not in the log", i, r.Seq)
		}
		w := want[r.Seq-1]
		got, _ := r.Canonical()
		exp, _ := w.Canonical()
		if !bytes.Equal(got, exp) || r.Prev != w.Prev || r.Hash != w.Hash {
			e.t.Errorf("record %d, seq %d:\n%s\nwant\n%s", i, r.Seq, b, exp)
		}
		switch {
		case r.Seq <= last: // a repeat
		case r.Seq == last+1:
			v.Event(r)
			last = r.Seq
		default:
			e.t.Errorf("record %d: seq %d after %d: a gap", i, r.Seq, last)
			last = r.Seq
		}
	}
	if last != int64(len(want)) {
		e.t.Errorf("the destination has the events up to seq %d, the log %d", last, len(want))
	}
	if res := v.Result(); res.Broken != nil || res.Events != int64(len(want)) {
		e.t.Errorf("the SIEM's copy verifies as %+v", res)
	}
	if !bytes.Contains(bytes.Join(raw, nil), []byte(`"pd:`)) {
		e.t.Error("no pseudonym in the records")
	}
}

func syslogDest(name, addr, transport string) config.AuditExport {
	f := config.DefaultSyslogFacility
	return config.AuditExport{Name: name, Syslog: &config.SyslogExport{Address: addr, Transport: transport, Facility: &f}}
}

// TestSyslog: the messages are RFC 5424, framed by octet counting, over TLS and TCP; a receiver
// that is down, or dies reading a batch, gets every event once it is back.
func TestSyslog(t *testing.T) {
	for _, transport := range []string{config.SyslogTLS, config.SyslogTCP} {
		t.Run(transport, func(t *testing.T) {
			e := setup(t)
			var cfg = e.ca.server
			if transport == config.SyslogTCP {
				cfg = nil
			}
			r := newSyslogReceiver(t, cfg)
			x := e.exporter(e.network(e.ca.pem), syslogDest("siem", r.addr, transport))
			e.record(3)
			if out, err := x.Export(ctx, "siem"); err != nil || out.Events != 3 || out.From != 1 || out.To != 3 {
				t.Fatalf("export = %+v, %v", out, err)
			}
			if out, err := x.Export(ctx, "siem"); err != nil || out.Events != 0 {
				t.Errorf("export with nothing new = %+v, %v", out, err)
			}
			msgs, errs := r.received()
			if len(errs) > 0 || len(msgs) != 3 {
				t.Fatalf("received %d messages, errors %v", len(msgs), errs)
			}
			ev := e.events()
			for i, m := range msgs {
				at, _ := time.Parse(audit.AtFormat, ev[i].At)
				if m.Version != 1 || *m.Facility != 13 || *m.Severity != 5 || !m.Timestamp.Equal(at) || *m.Hostname != "worker-1" ||
					*m.Appname != "quai" || m.ProcID != nil || *m.MsgID != "request.submitted" || m.StructuredData != nil {
					t.Errorf("message %d: %+v", i, m)
				}
				for _, c := range *m.Message {
					if c > 126 {
						t.Errorf("message %d is not ASCII: %q", i, *m.Message)
						break
					}
				}
			}

			// Down: nothing moves; back: every event.
			r.stop()
			e.record(2)
			if _, err := x.Export(ctx, "siem"); err == nil || !strings.Contains(err.Error(), "events 4 to 5") {
				t.Errorf("export to a receiver that is down: %v", err)
			}
			if c := e.cursor("siem"); c != 3 {
				t.Errorf("cursor after a failure = %d", c)
			}
			r.start()
			// A receiver dying after the first message of the batch: the batch comes again.
			r.resetAfter(1)
			if _, err := x.Export(ctx, "siem"); err == nil {
				t.Error("a batch the receiver did not read all of counts as delivered")
			}
			if c := e.cursor("siem"); c != 3 {
				t.Errorf("cursor after a reset = %d", c)
			}
			if out, err := x.Export(ctx, "siem"); err != nil || out.From != 4 || out.To != 5 {
				t.Fatalf("export once back = %+v, %v", out, err)
			}
			msgs, errs = r.received()
			if len(errs) > 0 {
				t.Errorf("errors %v", errs)
			}
			raw := make([][]byte, len(msgs))
			for i, m := range msgs {
				raw[i] = []byte(*m.Message)
			}
			if len(raw) != 6 { // 3, then 4 again, then 4 and 5
				t.Errorf("received %d messages", len(raw))
			}
			e.delivered(raw)
		})
	}
}

// TestOTLP: the records are OpenTelemetry log records, with the record in their body and its seq,
// hash and action as attributes, and the header from the secret store; a collector answering
// 503 gets every event once it is back; a partial success moves on, as OTLP requires.
func TestOTLP(t *testing.T) {
	e := setup(t)
	e.vars["OTLP_AUTH"] = "Bearer s3cr3t-otlp"
	r := newOTLPReceiver(t, e.ca.server)
	dest := config.AuditExport{Name: "otel", OTLP: &config.OTLPExport{Endpoint: r.URL, Headers: map[string]config.SecretRef{"Authorization": {Ref: "env:OTLP_AUTH"}}}}
	x := e.exporter(e.network(e.ca.pem), dest)
	e.record(2)
	if out, err := x.Export(ctx, "otel"); err != nil || out.Events != 2 {
		t.Fatalf("export = %+v, %v", out, err)
	}
	r.setDown(true)
	e.record(2)
	if _, err := x.Export(ctx, "otel"); err == nil || !strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("export to a collector that is down: %v", err)
	}
	if c := e.cursor("otel"); c != 2 {
		t.Errorf("cursor after a failure = %d", c)
	}
	r.setDown(false)
	if out, err := x.Export(ctx, "otel"); err != nil || out.From != 3 || out.To != 4 {
		t.Fatalf("export once back = %+v, %v", out, err)
	}
	recs, reqs, errs := r.records()
	if len(errs) > 0 || len(reqs) != 2 {
		t.Fatalf("%d requests, errors %v", len(reqs), errs)
	}
	for _, h := range r.headers {
		if h.Get("Authorization") != "Bearer s3cr3t-otlp" {
			t.Errorf("Authorization = %q", h.Get("Authorization"))
		}
	}
	res := reqs[0].GetResourceLogs()[0]
	attrs := map[string]string{}
	for _, kv := range res.GetResource().GetAttributes() {
		attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	if attrs["service.name"] != "quai" || attrs["host.name"] != "worker-1" || attrs["service.version"] != "test" || res.GetScopeLogs()[0].GetScope().GetName() != siem.Scope {
		t.Errorf("resource %v, scope %v", attrs, res.GetScopeLogs()[0].GetScope())
	}
	ev := e.events()
	raw := make([][]byte, len(recs))
	for i, lr := range recs {
		raw[i] = []byte(lr.GetBody().GetStringValue())
		at, _ := time.Parse(audit.AtFormat, ev[i].At)
		a := map[string]any{}
		for _, kv := range lr.GetAttributes() {
			if kv.GetKey() == siem.AttrSeq {
				a[kv.GetKey()] = kv.GetValue().GetIntValue()
			} else {
				a[kv.GetKey()] = kv.GetValue().GetStringValue()
			}
		}
		if lr.GetTimeUnixNano() != uint64(at.UnixNano()) || lr.GetSeverityText() != "NOTICE" || lr.GetSeverityNumber() != 10 ||
			a[siem.AttrSeq] != ev[i].Seq || a[siem.AttrHash] != ev[i].Hash || a[siem.AttrAction] != ev[i].Action {
			t.Errorf("record %d: %v", i, lr)
		}
	}
	e.delivered(raw)

	// The collector drops some records of a batch: OTLP says not to send them again.
	r.mu.Lock()
	r.reject = 1
	r.mu.Unlock()
	e.record(2)
	if out, err := x.Export(ctx, "otel"); err == nil || !strings.Contains(err.Error(), `rejected 1 of 2 records: "too large"`) || out.To != 6 {
		t.Errorf("export with a partial success = %+v, %v", out, err)
	}
	if c := e.cursor("otel"); c != 6 {
		t.Errorf("cursor after a partial success = %d", c)
	}
}

// TestSplunk: the events reach HEC with the token from the secret store, their time, host,
// source, index and sourcetype; a busy collector gets every event once it is not; its answers
// name what failed, never the token.
func TestSplunk(t *testing.T) {
	e := setup(t)
	hec := splunktest.New(t)
	hec.Indexes = []string{"audit"}
	e.vars["HEC_TOKEN"] = hec.Token
	dest := config.AuditExport{Name: "splunk", Splunk: &config.SplunkExport{URL: hec.URL, Token: config.SecretRef{Ref: "env:HEC_TOKEN"}, Index: "audit", Sourcetype: "quai:audit"}}
	cert := pemOf(hec.Certificate().Raw)
	x := e.exporter(e.network(cert), dest)
	e.record(2)
	if out, err := x.Export(ctx, "splunk"); err != nil || out.Events != 2 {
		t.Fatalf("export = %+v, %v", out, err)
	}
	hec.Answer(&splunktest.ServerBusy)
	e.record(3)
	if _, err := x.Export(ctx, "splunk"); err == nil || !strings.Contains(err.Error(), `"Server is busy" (code 9)`) {
		t.Errorf("export to a busy collector: %v", err)
	}
	hec.Answer(nil)
	if out, err := x.Export(ctx, "splunk"); err != nil || out.From != 3 || out.To != 5 {
		t.Fatalf("export once back = %+v, %v", out, err)
	}
	got := hec.Events()
	ev := e.events()
	raw := make([][]byte, len(got))
	for i, g := range got {
		raw[i] = g.Event
		at, _ := time.Parse(audit.AtFormat, ev[i].At)
		// The time is epoch seconds and milliseconds.
		if want := fmt.Sprintf("%d.%03d", at.Unix(), at.Nanosecond()/int(time.Millisecond)); g.Time.String() != want {
			t.Errorf("event %d: time %s, want %s", i, g.Time, want)
		}
		if g.Host != "worker-1" || g.Source != "quai" || g.Index != "audit" || g.Sourcetype != "quai:audit" {
			t.Errorf("event %d: %+v", i, g)
		}
	}
	e.delivered(raw)

	// A token the collector refuses.
	e.vars["WRONG_TOKEN"] = "not-the-token"
	dest.Splunk.Token.Ref = "env:WRONG_TOKEN"
	x = e.exporter(e.network(cert), dest)
	e.record(1)
	if _, err := x.Export(ctx, "splunk"); err == nil || !strings.Contains(err.Error(), `"Invalid token" (code 4)`) || strings.Contains(err.Error(), "not-the-token") {
		t.Errorf("export with a wrong token: %v", err)
	}
}

// TestUntrustedCertificate: a destination whose certificate no trusted CA signed is refused, and
// nothing is sent to it.
func TestUntrustedCertificate(t *testing.T) {
	e := setup(t)
	other := newAuthority(t) // trusted, but not the receivers' CA
	sys := newSyslogReceiver(t, e.ca.server)
	otlp := newOTLPReceiver(t, e.ca.server)
	hec := splunktest.New(t)
	e.vars["HEC_TOKEN"] = hec.Token
	x := e.exporter(e.network(other.pem),
		syslogDest("syslog", sys.addr, config.SyslogTLS),
		config.AuditExport{Name: "otel", OTLP: &config.OTLPExport{Endpoint: otlp.URL}},
		config.AuditExport{Name: "splunk", Splunk: &config.SplunkExport{URL: hec.URL, Token: config.SecretRef{Ref: "env:HEC_TOKEN"}}})
	e.record(1)
	for _, name := range []string{"syslog", "otel", "splunk"} {
		if _, err := x.Export(ctx, name); err == nil || !strings.Contains(err.Error(), "certificate signed by unknown authority") {
			t.Errorf("%s: %v", name, err)
		}
		if c := e.cursor(name); c != 0 {
			t.Errorf("%s: cursor %d", name, c)
		}
	}
	if msgs, _ := sys.received(); len(msgs) != 0 {
		t.Errorf("syslog received %d messages", len(msgs))
	}
	if recs, _, _ := otlp.records(); len(recs) != 0 {
		t.Errorf("OTLP received %d records", len(recs))
	}
	if hec.Requests() != 0 || len(hec.Events()) != 0 {
		t.Errorf("HEC received %d requests", hec.Requests())
	}
}

// TestRedirect: a destination answering with a redirect is not followed, so the credentials'
// headers go nowhere else.
func TestRedirect(t *testing.T) {
	e := setup(t)
	e.vars["OTLP_KEY"] = "s3cr3t-key"
	target := newOTLPReceiver(t, e.ca.server)
	redirect := httptest.NewUnstartedServer(http.RedirectHandler(target.URL+"/v1/logs", http.StatusTemporaryRedirect))
	redirect.TLS = e.ca.server
	redirect.StartTLS()
	defer redirect.Close()
	x := e.exporter(e.network(e.ca.pem), config.AuditExport{Name: "otel", OTLP: &config.OTLPExport{Endpoint: redirect.URL,
		Headers: map[string]config.SecretRef{"X-Api-Key": {Ref: "env:OTLP_KEY"}}}})
	e.record(1)
	if _, err := x.Export(ctx, "otel"); err == nil || !strings.Contains(err.Error(), "307") {
		t.Errorf("export to a redirect: %v", err)
	}
	if _, reqs, _ := target.records(); len(reqs) != 0 || e.cursor("otel") != 0 {
		t.Errorf("the redirect was followed: %d requests, cursor %d", len(reqs), e.cursor("otel"))
	}
}

// TestLag: the lag is how long ago the database recorded the oldest event a destination has not
// taken, and how many there are; nothing once it has them all.
func TestLag(t *testing.T) {
	e := setup(t)
	r := newSyslogReceiver(t, e.ca.server)
	x := e.exporter(e.network(e.ca.pem), syslogDest("siem", r.addr, config.SyslogTLS), syslogDest("later", "127.0.0.1:1", config.SyslogTLS))
	e.record(3)
	// Recorded ten minutes ago, as the schema's owner: the application role cannot set it.
	owner, err := pgxpool.New(ctx, pgtest.OwnerURL(e.url))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.Exec(ctx, "UPDATE audit_events SET recorded = now() - interval '10 minutes'"); err != nil {
		t.Fatal(err)
	}
	lags, err := x.Lag(ctx)
	if err != nil || len(lags) != 2 || lags[0].Destination != "later" || lags[1].Destination != "siem" {
		t.Fatalf("lag = %+v, %v", lags, err)
	}
	for _, l := range lags {
		if l.Pending != 3 || l.Lag < 10*time.Minute || l.Lag > 11*time.Minute {
			t.Errorf("before the export: %+v", l)
		}
	}
	if _, err := x.Export(ctx, "siem"); err != nil {
		t.Fatal(err)
	}
	lags, err = x.Lag(ctx)
	if err != nil || lags[1].Pending != 0 || lags[1].Lag != 0 || lags[0].Pending != 3 {
		t.Errorf("after the export = %+v, %v", lags, err)
	}
}

// TestPrunedBeforeExport: events pruned before a destination took them are a gap, which the
// worker logs as an error; the export goes on from the first event kept.
func TestPrunedBeforeExport(t *testing.T) {
	e := setup(t)
	r := newSyslogReceiver(t, e.ca.server)
	x := e.exporter(e.network(e.ca.pem), syslogDest("siem", r.addr, config.SyslogTLS))
	e.record(1)
	if _, err := x.Export(ctx, "siem"); err != nil {
		t.Fatal(err)
	}
	e.record(3)
	owner, err := pgxpool.New(ctx, pgtest.OwnerURL(e.url))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.Exec(ctx, "DELETE FROM audit_events WHERE seq IN (2, 3)"); err != nil {
		t.Fatal(err)
	}
	if out, err := x.Export(ctx, "siem"); err != nil || out.From != 4 || out.To != 4 {
		t.Fatalf("export = %+v, %v", out, err)
	}
	if !strings.Contains(e.log.String(), "audit events were pruned before they were exported") || !strings.Contains(e.log.String(), "from=2 to=3") {
		t.Errorf("log:\n%s", e.log)
	}
}

// TestChangedAfterExport: the event after the last one a destination took does not link to it,
// as after a restore from an older backup: the worker logs an error and goes on.
func TestChangedAfterExport(t *testing.T) {
	e := setup(t)
	r := newSyslogReceiver(t, e.ca.server)
	x := e.exporter(e.network(e.ca.pem), syslogDest("siem", r.addr, config.SyslogTLS))
	e.record(3)
	if _, err := x.Export(ctx, "siem"); err != nil {
		t.Fatal(err)
	}
	owner, err := pgxpool.New(ctx, pgtest.OwnerURL(e.url))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// The database lost event 3, and recorded others since: the next to export is 4, after a new 3.
	if _, err := owner.Exec(ctx, "DELETE FROM audit_events WHERE seq = 3"); err != nil {
		t.Fatal(err)
	}
	e.record(2)
	if out, err := x.Export(ctx, "siem"); err != nil || out.From != 4 || out.To != 4 {
		t.Fatalf("export = %+v, %v", out, err)
	}
	if !strings.Contains(e.log.String(), "the audit log changed after it was exported") || !strings.Contains(e.log.String(), "seq=4") {
		t.Errorf("log:\n%s", e.log)
	}
}

// TestJob: the worker exports each destination in a periodic job, from its start.
func TestJob(t *testing.T) {
	e := setup(t)
	r := newSyslogReceiver(t, e.ca.server)
	x := e.exporter(e.network(e.ca.pem), syslogDest("siem", r.addr, config.SyslogTLS))
	e.record(2)
	if len(x.Periodic()) != 1 {
		t.Fatalf("%d periodic jobs", len(x.Periodic()))
	}
	c, err := jobs.NewWorker(e.store.Pool, jobs.Options{Workers: x.Workers, Periodic: x.Periodic()})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Stop(ctx) }()
	for deadline := time.Now().Add(20 * time.Second); e.cursor("siem") != 2; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the job did not export")
		}
	}
	if msgs, errs := r.received(); len(msgs) != 2 || len(errs) > 0 {
		t.Errorf("received %d messages, errors %v", len(msgs), errs)
	}
}
