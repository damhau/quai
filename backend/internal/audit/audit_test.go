package audit_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var ctx = context.Background()

func record(t *testing.T, s *store.Store, events ...audit.Event) {
	t.Helper()
	if err := s.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error { return audit.Add(ctx, tx, events...) }); err != nil {
		t.Fatal(err)
	}
}

func all(t *testing.T, s *store.Store) []db.AuditEvent {
	t.Helper()
	rows, err := s.AuditEventsFrom(ctx, db.AuditEventsFromParams{Seq: 1, MaxRows: 100000})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestCanonical(t *testing.T) {
	// The form cannot change once a chain exists (open question 18): RFC 8785, keys sorted,
	// no spaces, numbers and strings as ECMAScript writes them.
	e := db.AuditEvent{Seq: 1, At: pgtype.Timestamptz{Time: time.Date(2026, 10, 26, 11, 40, 12, 331_000_000, time.FixedZone("CEST", 2*3600)), Valid: true},
		Actor: json.RawMessage(`{"type": "system", "id": "quai"}`), Action: "request.approved",
		Object:  json.RawMessage(`{"type":"request","id":"r1","resource":"infra-live/vm/prod/web01"}`),
		Details: json.RawMessage(`{"b": 2.50, "a": "é<", "c": [1e2, true, null]}`), Prev: audit.Genesis}
	c, err := audit.Canonical(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"action":"request.approved","actor":{"id":"quai","type":"system"},"at":"2026-10-26T09:40:12.331Z",` +
		`"details":{"a":"é<","b":2.5,"c":[100,true,null]},"object":{"id":"r1","resource":"infra-live/vm/prod/web01","type":"request"},"seq":1}`
	if string(c) != want {
		t.Errorf("canonical =\n%s\nwant\n%s", c, want)
	}
	sum := sha256.Sum256([]byte(audit.Genesis + want))
	if h, err := audit.Hash(e); err != nil || h != hex.EncodeToString(sum[:]) {
		t.Errorf("hash = %s, %v", h, err)
	}
}

func TestRecord(t *testing.T) {
	s := pgtest.Store(t)
	at := time.Date(2026, 10, 26, 9, 38, 55, 0, time.UTC)
	actx := audit.WithActor(ctx, audit.Actor{Type: audit.User, ID: "alice", IP: "10.1.2.3", AuthTime: at})
	err := s.Tx(actx, func(tx pgx.Tx, _ *db.Queries) error {
		return audit.Add(actx, tx, audit.Event{Action: "request.reviewed", Object: audit.Object{Type: "request", ID: "r1", Resource: "infra-live/vm/prod/web01"},
			Details: map[string]any{"decision": "approve", "comment": audit.Personal("looks good\x00, \xff"), "approvals": []audit.Personal{"alice", "bob"},
				"nested": []map[string]any{{"who": audit.Personal("carol")}}, "empty": audit.Personal("")}})
	})
	if err != nil {
		t.Fatal(err)
	}
	record(t, s, audit.Event{Action: "repository.created", Object: audit.Object{Type: "repository", ID: "infra-live"}},
		audit.Event{Actor: audit.Actor{Type: audit.Token, ID: "bob", Token: "t1"}, Action: "token.revoked", Object: audit.Object{Type: "user", ID: "bob"}})

	rows := all(t, s)
	if len(rows) != 3 || rows[0].Seq != 1 || rows[0].Prev != audit.Genesis || rows[1].Prev != rows[0].Hash || rows[2].Prev != rows[1].Hash {
		t.Fatalf("events = %+v", rows)
	}
	var actor map[string]string
	_ = json.Unmarshal(rows[0].Actor, &actor)
	if actor["type"] != "user" || !strings.HasPrefix(actor["id"], audit.PersonalPrefix) || !strings.HasPrefix(actor["ip"], audit.PersonalPrefix) ||
		actor["authTime"] != "2026-10-26T09:38:55Z" {
		t.Errorf("actor = %v", actor)
	}
	if strings.Contains(string(rows[0].Actor)+string(rows[0].Details), "alice") || strings.Contains(string(rows[0].Details), "good") {
		t.Errorf("personal data in the event: %s %s", rows[0].Actor, rows[0].Details)
	}
	if !strings.Contains(string(rows[0].Details), `"decision": "approve"`) || !strings.Contains(string(rows[0].Details), `"empty": ""`) {
		t.Errorf("details = %s", rows[0].Details)
	}
	// The system acts when no one is in the context; a user object is personal.
	var system, object map[string]string
	_ = json.Unmarshal(rows[1].Actor, &system)
	_ = json.Unmarshal(rows[2].Object, &object)
	if system["type"] != "system" || system["id"] != "quai" || !strings.HasPrefix(object["id"], audit.PersonalPrefix) {
		t.Errorf("actor = %v, object = %v", system, object)
	}
	if rows[1].At.Time.Before(rows[0].At.Time) || rows[1].At.Time.Nanosecond()%int(time.Millisecond) != 0 {
		t.Errorf("times = %v, %v", rows[0].At.Time, rows[1].At.Time)
	}

	// The clear values are apart, each explaining its pseudonym.
	personal, err := s.AuditPersonalOf(ctx, []int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, p := range personal {
		if !audit.PersonalHolds(p.Hash, p.Salt, p.Value) {
			t.Errorf("%s does not hold %q", p.Hash, p.Value)
		}
		values[p.Hash] = p.Value
	}
	if values[actor["id"]] != "alice" || values[actor["ip"]] != "10.1.2.3" || values[object["id"]] != "bob" || len(personal) != 8 {
		t.Errorf("personal = %v", values)
	}
	var details struct {
		Comment   string   `json:"comment"`
		Approvals []string `json:"approvals"`
	}
	_ = json.Unmarshal(rows[0].Details, &details)
	if values[details.Comment] != "looks good, �" || len(details.Approvals) != 2 || values[details.Approvals[1]] != "bob" || details.Approvals[0] == details.Approvals[1] {
		t.Errorf("details = %+v, values %v", details, values)
	}

	if n, broken, err := audit.Verify(ctx, s.Queries); err != nil || broken != nil || n != 3 {
		t.Errorf("verify = %d, %v, %v", n, broken, err)
	}
}

func TestInTheTransaction(t *testing.T) {
	s := pgtest.Store(t)
	// An event belongs to what it records: rolled back with it.
	failed := errors.New("failed")
	err := s.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		if err := audit.Add(ctx, tx, audit.Event{Action: "token.created", Object: audit.Object{Type: "token", ID: "t1"}}); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatal(err)
	}
	record(t, s, audit.Event{Action: "token.created", Object: audit.Object{Type: "token", ID: "t2"}})
	if rows := all(t, s); len(rows) != 1 || rows[0].Seq != 1 || !strings.Contains(string(rows[0].Object), "t2") {
		t.Errorf("events = %+v", rows)
	}
	// Only a transaction store.Tx opens records.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := audit.Add(ctx, tx, audit.Event{Action: "x"}); !errors.Is(err, audit.ErrNotRecording) {
		t.Errorf("Add outside store.Tx: %v", err)
	}
}

func TestConcurrentWriters(t *testing.T) {
	s := pgtest.Store(t)
	const writers, each = 12, 15
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				err := s.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
					// The transaction's own work, then its events.
					if _, err := q.AddEvent(ctx, db.AddEventParams{Target: fmt.Sprintf("w%d", w), Kind: "work", Data: []byte(`{}`)}); err != nil {
						return err
					}
					evs := []audit.Event{{Action: "request.submitted", Object: audit.Object{Type: "request", ID: fmt.Sprintf("%d-%d", w, i)}}}
					if i%3 == 0 {
						evs = append(evs, audit.Event{Action: "request.approved", Object: audit.Object{Type: "request", ID: fmt.Sprintf("%d-%d", w, i)}})
					}
					return audit.Add(ctx, tx, evs...)
				})
				if err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	rows := all(t, s)
	want := writers * (each + (each+2)/3)
	if len(rows) != want {
		t.Fatalf("%d events, want %d", len(rows), want)
	}
	for i, r := range rows {
		if r.Seq != int64(i+1) {
			t.Fatalf("seq %d at %d: a gap", r.Seq, i+1)
		}
		if i > 0 && (r.Prev != rows[i-1].Hash || r.At.Time.Before(rows[i-1].At.Time)) {
			t.Fatalf("event %d does not chain on %d", r.Seq, rows[i-1].Seq)
		}
	}
	if n, broken, err := audit.Verify(ctx, s.Queries); err != nil || broken != nil || n != int64(want) {
		t.Errorf("verify = %d, %v, %v", n, broken, err)
	}
}

func TestInsertOnly(t *testing.T) {
	s := pgtest.Store(t)
	for i := range 4 {
		record(t, s, audit.Event{Actor: audit.Actor{Type: audit.User, ID: "alice"}, Action: "auth.login", Object: audit.Object{Type: "user", ID: "alice"},
			Details: map[string]any{"n": i}})
	}
	// The application role reads and appends; it changes nothing.
	for _, stmt := range []string{
		"UPDATE audit_events SET details = '{}' WHERE seq = 2",
		"DELETE FROM audit_events WHERE seq = 4",
		"TRUNCATE audit_events",
		"UPDATE audit_personal SET value = 'mallory'",
		"DELETE FROM audit_personal",
		"TRUNCATE audit_personal",
		"ALTER TABLE audit_events DISABLE TRIGGER ALL",
		"DROP TABLE audit_events",
		"UPDATE goose_db_version SET is_applied = false",
	} {
		var pgErr *pgconn.PgError
		if _, err := s.Pool.Exec(ctx, stmt); !errors.As(err, &pgErr) || (pgErr.Code != "42501" && pgErr.Code != "42809") {
			t.Errorf("as the application role, %s: %v", stmt, err)
		}
	}
	if p, err := s.AuditProtected(ctx); err != nil || !p.Bool {
		t.Errorf("protected as the application role = %v, %v", p, err)
	}
	if n, broken, err := audit.Verify(ctx, s.Queries); err != nil || broken != nil || n != 4 {
		t.Errorf("verify = %d, %v, %v", n, broken, err)
	}

	// The owner can change a row, and the chain shows where.
	owner, err := pgxpool.New(ctx, pgtest.OwnerURL(s.Pool.Config().ConnString()))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	oq := db.New(owner)
	if p, err := oq.AuditProtected(ctx); err != nil || p.Bool {
		t.Errorf("protected as the owner = %v, %v", p, err)
	}
	for _, c := range []struct {
		stmt string
		seq  int64
	}{
		{`UPDATE audit_events SET details = '{"n": 7}' WHERE seq = 3`, 3},
		{`UPDATE audit_events SET details = '{"n": 2}', hash = 'f00' WHERE seq = 3`, 3},
		{`UPDATE audit_events SET at = at + interval '1 second' WHERE seq = 2`, 2},
		{`DELETE FROM audit_events WHERE seq = 2`, 2},
	} {
		if _, err := owner.Exec(ctx, c.stmt); err != nil {
			t.Fatal(err)
		}
		_, broken, err := audit.Verify(ctx, s.Queries)
		if err != nil || broken == nil || broken.Seq != c.seq {
			t.Errorf("after %s: %v, %v", c.stmt, broken, err)
		}
	}
}

func TestContext(t *testing.T) {
	if a := audit.ActorFrom(ctx); a != audit.Quai {
		t.Errorf("default actor = %+v", a)
	}
	c := audit.WithIP(audit.WithActor(ctx, audit.Actor{Type: audit.Runner, ID: "infra-live"}), "10.0.0.1")
	if a := audit.ActorFrom(c); a.Type != audit.Runner || audit.IPFrom(c) != "10.0.0.1" || audit.IPFrom(ctx) != "" {
		t.Errorf("actor = %+v, ip = %q", a, audit.IPFrom(c))
	}
	b := &audit.Broken{Seq: 3, Reason: "x"}
	if !strings.Contains(b.Error(), "seq 3") {
		t.Error(b.Error())
	}
}
