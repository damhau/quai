// Package siem exports the audit log to SIEMs (docs/design/09-security-and-compliance.md#export-to-a-siem):
// syslog (RFC 5424 over TLS or TCP), OpenTelemetry logs (OTLP/HTTP) and Splunk's HTTP Event
// Collector, as audit.export configures them.
//
// The worker exports each destination every Every, in a job of its own: it sends the events after
// the destination's cursor in seq order, a batch at a time, and moves the cursor to the last event
// of a batch once the destination took it. An event is sent at least once: a batch the destination
// did not take, or took without the cursor moving (a worker stopped between the two), is sent
// again. Each record is the event as the log holds it, with its seq, prev and hash, and personal
// data as pseudonyms, so the SIEM can see a gap or a repeat and verify the chain.
//
// Connections are verified against the system's certificates and network.caFile: there is no
// way to turn verification off. Credentials are secret references, resolved at each send and
// never logged.
package siem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
	"github.com/damhau/quai/backend/internal/telemetry"
)

// Every is how often each destination is exported to; a run sends batches of Batch events until
// the destination has them all or Budget has passed, and a run that fails is tried again at the
// next.
const (
	Every  = 30 * time.Second
	Batch  = 500
	Budget = time.Minute
)

// Timeout bounds one send: connecting, sending a batch and the destination's answer.
const Timeout = 30 * time.Second

// Options configures an exporter.
type Options struct {
	Store        *store.Store
	Destinations []config.AuditExport
	Secrets      *secrets.Store
	// Network holds the certificates to trust and the proxy; nil: the system's certificates and
	// no proxy.
	Network *netx.Network
	Log     *slog.Logger
	// Hostname is the host the records come from; os.Hostname() when empty.
	Hostname string
	// Service and Version name Quai in OpenTelemetry's resource: quai and its version.
	Service, Version string
}

// Exporter exports the audit log to its destinations.
type Exporter struct {
	o     Options
	dests map[string]sender
	names []string
}

// sender delivers records to a destination, in order: nil only when it took them all.
type sender interface {
	send(ctx context.Context, records []audit.Record) error
}

// rejected is a destination's answer that it took a batch but dropped some of its records, which
// the protocol says not to send again (an OTLP partial success): the cursor moves past them, and
// the run ends with this error.
type rejected struct{ msg string }

func (r *rejected) Error() string { return r.msg }

// New returns an exporter of the destinations.
func New(o Options) (*Exporter, error) {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Hostname == "" {
		o.Hostname, _ = os.Hostname()
	}
	if o.Service == "" {
		o.Service = "quai"
	}
	if o.Network == nil {
		n, err := netx.New("")
		if err != nil {
			return nil, err
		}
		o.Network = n
	}
	e := &Exporter{o: o, dests: map[string]sender{}}
	// A redirect is an error, not followed: the client would send the credentials' headers on.
	client := &http.Client{Transport: o.Network.Transport(), Timeout: Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, d := range o.Destinations {
		switch {
		case d.Syslog != nil:
			e.dests[d.Name] = newSyslog(*d.Syslog, o.Network, o.Hostname)
		case d.OTLP != nil:
			e.dests[d.Name] = newOTLP(*d.OTLP, client, o.Secrets, o.Hostname, o.Service, o.Version)
		case d.Splunk != nil:
			e.dests[d.Name] = newSplunk(*d.Splunk, client, o.Secrets, o.Hostname)
		default:
			return nil, fmt.Errorf("audit.export %s: no syslog, otlp or splunk", d.Name)
		}
		e.names = append(e.names, d.Name)
	}
	return e, nil
}

// Exported is what a run sent: the events from seq From to To, Events of them.
type Exported struct {
	From, To int64
	Events   int
}

// Export sends a destination the events after its cursor, in seq order, a batch at a time, until
// it has them all, Budget has passed or it fails; the cursor moves after each batch it takes.
func (e *Exporter) Export(ctx context.Context, name string) (Exported, error) {
	var out Exported
	dest, ok := e.dests[name]
	if !ok {
		return out, fmt.Errorf("no audit export destination %s", name)
	}
	after, prev := int64(0), ""
	cur, err := e.o.Store.AuditExportCursor(ctx, name)
	switch {
	case err == nil:
		after, prev = cur.Seq, cur.Hash
	case !errors.Is(err, pgx.ErrNoRows):
		return out, err
	}
	deadline := time.Now().Add(Budget)
	for {
		rows, err := e.o.Store.AuditEventsFrom(ctx, db.AuditEventsFromParams{Seq: after + 1, MaxRows: Batch})
		if err != nil || len(rows) == 0 {
			return out, err
		}
		switch {
		case after > 0 && rows[0].Seq > after+1:
			// Pruning deleted them first: the export was behind by more than the retention.
			e.o.Log.Error("audit events were pruned before they were exported: the SIEM will miss them",
				slog.String("destination", name), slog.Int64("from", after+1), slog.Int64("to", rows[0].Seq-1))
		case prev != "" && rows[0].Prev != prev:
			// The event after the last one exported does not link to it: the log changed since,
			// restored from a backup or rewritten. The SIEM sees the chain break there too.
			e.o.Log.Error("the audit log changed after it was exported: the next event does not link to the last one the SIEM has "+
				"(docs/operations/deployment-and-operations.md#when-the-chain-does-not-verify)",
				slog.String("destination", name), slog.Int64("seq", rows[0].Seq))
		}
		records := make([]audit.Record, len(rows))
		for i, r := range rows {
			records[i] = audit.RecordOf(r)
		}
		sendErr := dest.send(ctx, records)
		var rej *rejected
		if sendErr != nil && !errors.As(sendErr, &rej) {
			return out, fmt.Errorf("the audit export to %s, events %d to %d: %w", name, records[0].Seq, records[len(records)-1].Seq, sendErr)
		}
		last := records[len(records)-1]
		if err := e.o.Store.AdvanceAuditExport(ctx, db.AdvanceAuditExportParams{Destination: name, Seq: last.Seq, Hash: last.Hash}); err != nil {
			return out, err
		}
		if out.Events == 0 {
			out.From = records[0].Seq
		}
		out.To, out.Events, after, prev = last.Seq, out.Events+len(records), last.Seq, last.Hash
		if sendErr != nil {
			return out, fmt.Errorf("the audit export to %s, events %d to %d: %w", name, records[0].Seq, last.Seq, sendErr)
		}
		if len(rows) < Batch || time.Now().After(deadline) {
			return out, nil
		}
	}
}

// Lag returns how far each destination is behind, for the metrics.
func (e *Exporter) Lag(ctx context.Context) ([]telemetry.AuditExportLag, error) {
	rows, err := e.o.Store.AuditExportLag(ctx, e.names)
	if err != nil {
		return nil, err
	}
	lags := make([]telemetry.AuditExportLag, len(rows))
	for i, r := range rows {
		lags[i] = telemetry.AuditExportLag{Destination: r.Destination, Pending: r.Pending, Lag: time.Duration(r.Lag * float64(time.Second))}
	}
	return lags, nil
}

// ExportArgs exports the audit log to a destination; each destination's job runs every Every.
type ExportArgs struct {
	Destination string `json:"destination"`
}

// Kind names the job.
func (ExportArgs) Kind() string { return "audit_export" }

// Workers adds the exporter's worker.
func (e *Exporter) Workers(w *river.Workers) { river.AddWorker(w, &exportWorker{e: e}) }

// Periodic are the exporter's periodic jobs, one per destination, for jobs.Options.
func (e *Exporter) Periodic() []*river.PeriodicJob {
	var out []*river.PeriodicJob
	for _, name := range e.names {
		out = append(out, river.NewPeriodicJob(river.PeriodicInterval(Every), func() (river.JobArgs, *river.InsertOpts) {
			return ExportArgs{Destination: name}, nil
		}, &river.PeriodicJobOpts{RunOnStart: true}))
	}
	return out
}

type exportWorker struct {
	river.WorkerDefaults[ExportArgs]
	e *Exporter
}

// Timeout leaves a run its Budget and a last send.
func (w *exportWorker) Timeout(*river.Job[ExportArgs]) time.Duration { return Budget + 2*Timeout }

// Work exports a destination, unless another worker is exporting it already. A failed run is
// logged and counted, and the next tries again: River does not retry it.
func (w *exportWorker) Work(ctx context.Context, job *river.Job[ExportArgs]) error {
	name := job.Args.Destination
	if _, ok := w.e.dests[name]; !ok {
		return nil // a destination the configuration no longer has
	}
	_, err := jobs.TryLock(ctx, w.e.o.Store.Locks, "quai/audit/export/"+name, func(ctx context.Context) error {
		x, err := w.e.Export(ctx, name)
		telemetry.AuditExported(ctx, name, err)
		if err != nil {
			w.e.o.Log.Warn("the audit export failed; it is tried again in "+Every.String(), slog.String("destination", name), slog.Any("error", err))
		} else if x.Events > 0 {
			w.e.o.Log.Debug("audit events exported", slog.String("destination", name), slog.Int64("from", x.From), slog.Int64("to", x.To))
		}
		return nil
	})
	return err
}

// recordJSON is a record as each destination carries it: the event as the log holds it and
// `quai audit export` writes it.
func recordJSON(r audit.Record) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Severities of the records, as syslog numbers them (RFC 5424, section 6.2.1): what may need
// someone's attention is a warning, every other event a notice.
const (
	severityWarning = 4
	severityNotice  = 5
)

func severity(action string) int {
	switch action {
	case "audit.broken", "request.merged_without_approval", "repository.enforcement_lost":
		return severityWarning
	}
	return severityNotice
}

// printable keeps what RFC 5424 allows in a header field, printable US-ASCII, at most max
// characters; "-" (nothing) when none is left.
func printable(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 33 || r > 126 {
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max]
	}
	if s == "" {
		return "-"
	}
	return s
}
