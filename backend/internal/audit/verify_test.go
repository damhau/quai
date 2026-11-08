package audit_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// newKey returns a checkpoint signing key and the keys that verify it.
func newKey(t *testing.T) (ed25519.PrivateKey, audit.Keys) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := audit.Keys{}
	keys.Add(pub)
	return key, keys
}

// appendAt appends an event at each time, as a writer with its own clock would: the application
// role can, and the database records it now.
func appendAt(t *testing.T, q *db.Queries, times ...time.Time) {
	t.Helper()
	seq, prev := int64(0), audit.Genesis
	if last, err := q.LastAuditEvent(ctx); err == nil {
		seq, prev = last.Seq, last.Hash
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	for _, at := range times {
		seq++
		at = at.UTC().Truncate(time.Millisecond)
		r := audit.Record{Seq: seq, At: audit.Time(at), Actor: json.RawMessage(`{"id":"quai","type":"system"}`), Action: "test.old",
			Object: json.RawMessage(`{"id":"x","type":"test"}`), Details: json.RawMessage(`{}`), Prev: prev}
		h, err := r.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if err := q.InsertAuditEvent(ctx, db.InsertAuditEventParams{Seq: seq, At: pgtype.Timestamptz{Time: at, Valid: true}, Actor: r.Actor,
			Action: r.Action, Object: r.Object, Details: r.Details, Prev: prev, Hash: h}); err != nil {
			t.Fatal(err)
		}
		prev = h
	}
}

// sign signs a checkpoint of the latest event.
func sign(t *testing.T, s *store.Store, key ed25519.PrivateKey) audit.Checkpoint {
	t.Helper()
	c, fresh, err := audit.SignLatest(ctx, s.Queries, key, time.Now())
	if err != nil || !fresh {
		t.Fatalf("sign = %+v, %v, %v", c, fresh, err)
	}
	return c
}

// export returns the log's export, one JSON value a line.
func export(t *testing.T, s *store.Store) []string {
	t.Helper()
	var b bytes.Buffer
	if _, err := audit.Export(ctx, s.Pool, &b); err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
}

// verifyLines verifies an export, with anchors from the audit repository.
func verifyLines(t *testing.T, keys audit.Keys, lines []string, anchors ...audit.Checkpoint) audit.Result {
	t.Helper()
	v := audit.NewVerifier(keys)
	for _, a := range anchors {
		if err := v.Checkpoint(a, audit.FromRepository); err != nil {
			t.Fatal(err)
		}
	}
	if err := audit.ReadExport(strings.NewReader(strings.Join(lines, "\n")+"\n"), v); err != nil {
		t.Fatal(err)
	}
	return v.Result()
}

func login(i int) audit.Event {
	return audit.Event{Actor: audit.Actor{Type: audit.User, ID: "alice", IP: "10.0.0.1"}, Action: "auth.login",
		Object: audit.Object{Type: "user", ID: "alice"}, Details: map[string]any{"n": i}}
}

// TestExportVerifies: an export of the log verifies offline with the public key, and each
// tampering fails at the seq it touches.
func TestExportVerifies(t *testing.T) {
	s := pgtest.Store(t)
	key, keys := newKey(t)
	for i := range 4 {
		record(t, s, login(i))
	}
	c4 := sign(t, s, key)
	for i := range 4 {
		record(t, s, login(4+i))
	}
	c8 := sign(t, s, key)
	if again, fresh, err := audit.SignLatest(ctx, s.Queries, key, time.Now()); err != nil || fresh || again != c8 {
		t.Errorf("signing again with no new event = %+v, %v, %v", again, fresh, err)
	}
	record(t, s, login(8), login(9))

	lines := export(t, s)
	if len(lines) != 12 || !strings.HasPrefix(lines[0], `{"type":"checkpoint","seq":4,`) || !strings.HasPrefix(lines[2], `{"seq":1,`) {
		t.Fatalf("export:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(strings.Join(lines, ""), "alice") || strings.Contains(strings.Join(lines, ""), "10.0.0.1") {
		t.Error("the export holds clear personal values")
	}
	res := verifyLines(t, keys, lines)
	if res.Broken != nil || res.Events != 10 || res.Checkpoints != 2 || res.First != 1 || res.Last != 10 || res.Start != 0 {
		t.Errorf("export = %+v, %v", res, res.Broken)
	}
	// The same checkpoints from the audit repository count once.
	if res, err := audit.Verify(ctx, s.Pool, keys, c4, c8); err != nil || res.Broken != nil || res.Checkpoints != 2 {
		t.Errorf("verify with anchors = %+v, %v", res, err)
	}

	event := func(seq int) int { return 1 + seq } // the line of an event: after the two checkpoints
	edit := func(f func(l []string) []string) []string { return f(append([]string(nil), lines...)) }
	other, otherKeys := newKey(t)
	forged, _ := audit.Sign(other, c8.Seq, c8.Hash, time.Now())
	forged.Key = c8.Key
	reb, _ := json.Marshal(forged)
	for _, c := range []struct {
		name  string
		keys  audit.Keys
		lines []string
		seq   int64
		why   string
	}{
		{"a changed event", keys, edit(func(l []string) []string {
			l[event(6)] = strings.Replace(l[event(6)], `"n":5`, `"n":50`, 1)
			return l
		}), 6, "its hash is not the hash of its content"},
		{"a deleted event", keys, edit(func(l []string) []string { return append(l[:event(3)], l[event(3)+1:]...) }), 3, "missing"},
		{"the first event deleted", keys, edit(func(l []string) []string { return append(l[:event(1)], l[event(1)+1:]...) }), 1, "missing"},
		{"the last events deleted", keys, edit(func(l []string) []string { return l[:event(7)+1] }), 8, "the log ends at seq 7"},
		{"an inserted event", keys, edit(func(l []string) []string {
			return append(l[:event(5)], append([]string{forge(t, l[event(4)], 5)}, l[event(5):]...)...)
		}), 5, "comes again"},
		{"a reordered event", keys, edit(func(l []string) []string {
			l[event(5)], l[event(6)] = l[event(6)], l[event(5)]
			return l
		}), 5, "missing or out of order"},
		{"a rewritten chain", keys, edit(func(l []string) []string {
			// Seq 6 changed and every hash after it recomputed: only the checkpoint shows it.
			l[event(6)] = strings.Replace(l[event(6)], `"n":5`, `"n":50`, 1)
			return rehash(t, l, event(6))
		}), 8, "the one the checkpoint signed"},
		{"a checkpoint with a bad signature", keys, edit(func(l []string) []string { l[1] = string(reb); return l }), 8, "its signature does not verify"},
		{"a checkpoint with an unknown key", otherKeys, lines, 4, "which was not given"},
	} {
		res := verifyLines(t, c.keys, c.lines)
		if res.Broken == nil || res.Broken.Seq != c.seq || !strings.Contains(res.Broken.Reason, c.why) {
			t.Errorf("%s: %v, want seq %d (%s)", c.name, res.Broken, c.seq, c.why)
		}
	}

	// Pseudonymizing alice deletes her clear values (as the schema owner): the chain still holds.
	owner, err := pgxpool.New(ctx, pgtest.OwnerURL(s.Pool.Config().ConnString()))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if tag, err := owner.Exec(ctx, `DELETE FROM audit_personal WHERE value = 'alice'`); err != nil || tag.RowsAffected() != 20 {
		t.Fatalf("pseudonymizing alice: %v, %v", tag, err)
	}
	if res, err := audit.Verify(ctx, s.Pool, keys); err != nil || res.Broken != nil || res.Events != 10 {
		t.Errorf("verify after pseudonymizing = %+v, %v", res, err)
	}

	// A checkpoint in the audit repository beyond the log: the database lost events.
	record(t, s, login(10))
	c11 := sign(t, s, key)
	if res := verifyLines(t, keys, lines, c11); res.Broken == nil || res.Broken.Seq != 11 || !strings.Contains(res.Broken.Reason, "checkpoint in the audit repository says it reached seq 11") {
		t.Errorf("an anchor beyond the log = %v", res.Broken)
	}

	// What is not an export.
	for _, bad := range [][]string{
		{lines[2], lines[0]},
		{strings.Replace(lines[2], `"seq":1,`, `"seq":1,"extra":true,`, 1)},
		{`{"type":"note"}`},
		{`[1]`},
	} {
		v := audit.NewVerifier(keys)
		if err := audit.ReadExport(strings.NewReader(strings.Join(bad, "\n")), v); err == nil {
			t.Errorf("reading %q: no error", bad)
		}
	}
}

// forge returns the event after line, a well-formed one with seq.
func forge(t *testing.T, line string, seq int64) string {
	t.Helper()
	var prev audit.Record
	if err := json.Unmarshal([]byte(line), &prev); err != nil {
		t.Fatal(err)
	}
	r := prev
	r.Seq, r.Prev, r.Details = seq, prev.Hash, json.RawMessage(`{"n":"forged"}`)
	r.Hash, _ = r.Digest()
	b, _ := json.Marshal(r)
	return string(b)
}

// rehash recomputes the hashes of the events from a line on, as someone rewriting the chain would.
func rehash(t *testing.T, lines []string, from int) []string {
	t.Helper()
	var prev audit.Record
	_ = json.Unmarshal([]byte(lines[from-1]), &prev)
	for i := from; i < len(lines); i++ {
		var r audit.Record
		if err := json.Unmarshal([]byte(lines[i]), &r); err != nil {
			t.Fatal(err)
		}
		r.Prev = prev.Hash
		r.Hash, _ = r.Digest()
		b, _ := json.Marshal(r)
		lines[i], prev = string(b), r
	}
	return lines
}

// TestPrune: the application role prunes only through audit_prune, only what the database recorded
// before the retention and before a checkpoint that matches its event; the chain then verifies
// from it.
func TestPrune(t *testing.T) {
	s := pgtest.Store(t)
	key, keys := newKey(t)
	owner, err := pgxpool.New(ctx, pgtest.OwnerURL(s.Pool.Config().ConnString()))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	prune := func() audit.Pruned {
		t.Helper()
		var p audit.Pruned
		if err := s.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error { p, err = audit.Prune(ctx, tx); return err }); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// What the application says of an event's time: two years ago, a minute apart.
	old := time.Now().AddDate(-2, 0, 0)
	at := func(n int) []time.Time {
		var out []time.Time
		for range n {
			old = old.Add(time.Minute)
			out = append(out, old)
		}
		return out
	}
	// When the database recorded the events up to a seq: two years ago, as only the schema's
	// owner can say.
	age := func(upTo int64) {
		t.Helper()
		if _, err := owner.Exec(ctx, `UPDATE audit_events SET recorded = $1::timestamptz + make_interval(secs => seq) WHERE seq <= $2`,
			time.Now().AddDate(-2, 0, 0), upTo); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		var n int
		if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// audit_prune runs as its owner, with its own search path.
	var definer bool
	var settings []string
	if err := s.Pool.QueryRow(ctx, "SELECT prosecdef, proconfig FROM pg_proc WHERE oid = 'audit_prune()'::regprocedure").Scan(&definer, &settings); err != nil {
		t.Fatal(err)
	}
	if !definer || len(settings) != 1 || settings[0] != "search_path=public, pg_temp" {
		t.Errorf("audit_prune: security definer %v, %v", definer, settings)
	}

	// Events the application dated two years ago, with a checkpoint of the last, even with the
	// shortest retention: the database recorded them now, and nothing goes.
	appendAt(t, s.Queries, at(4)...)
	if err := s.InsertAuditPersonal(ctx, db.InsertAuditPersonalParams{Hash: "pd:x", Seq: 2, Salt: []byte("salt"), Value: "alice"}); err != nil {
		t.Fatal(err)
	}
	if p := prune(); p.Events != 0 {
		t.Errorf("pruned with no checkpoint: %+v", p)
	}
	// A checkpoint that does not match its event is no anchor.
	if _, err := owner.Exec(ctx, `INSERT INTO audit_checkpoints VALUES (3, repeat('0', 64), now(), 'k', 's')`); err != nil {
		t.Fatal(err)
	}
	if p := prune(); p.Events != 0 {
		t.Errorf("pruned before a checkpoint that is not its event's: %+v", p)
	}
	if _, err := owner.Exec(ctx, `DELETE FROM audit_checkpoints`); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAuditRetention(ctx, owner, config.MinAuditRetention); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAuditCheckpoint(ctx, db.InsertAuditCheckpointParams{Seq: 4, Hash: all(t, s)[3].Hash, At: pgtype.Timestamptz{Time: old, Valid: true},
		Key: "k", Signature: "s"}); err != nil {
		t.Fatal(err)
	}
	if p := prune(); p.Events != 0 || count() != 4 {
		t.Errorf("pruned events the application dated back: %+v, %d left", p, count())
	}
	if _, err := owner.Exec(ctx, `DELETE FROM audit_checkpoints`); err != nil {
		t.Fatal(err)
	}

	// Events up to 4 recorded two years ago, a checkpoint of 4, others after it, then recent ones
	// and their checkpoint at 8.
	age(4)
	sign(t, s, key)
	appendAt(t, s.Queries, at(3)...)
	age(7)
	record(t, s, login(0))
	sign(t, s, key)
	record(t, s, login(1))

	// The application role cannot delete, nor change the retention.
	for _, stmt := range []string{
		"DELETE FROM audit_events WHERE seq < 4",
		"DELETE FROM audit_checkpoints",
		"UPDATE audit_checkpoints SET hash = 'x'",
		"TRUNCATE audit_checkpoints",
		"UPDATE audit_retention SET retention = interval '1 second'",
		"INSERT INTO audit_retention VALUES (false, interval '1 second')",
		"DELETE FROM audit_retention",
	} {
		var pgErr *pgconn.PgError
		if _, err := s.Pool.Exec(ctx, stmt); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("as the application role, %s: %v", stmt, err)
		}
	}
	if err := store.SetAuditRetention(ctx, s.Pool, time.Hour); err == nil {
		t.Error("the application role set the retention")
	}
	// Nor shadow the retention with its own table: audit_prune reads the owner's (and this
	// transaction is rolled back).
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var shadowed int64
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE audit_retention (only_row boolean, retention interval) ON COMMIT DROP;
		INSERT INTO audit_retention VALUES (true, interval '1 microsecond'); SET LOCAL search_path = pg_temp, public`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, "SELECT coalesce(checkpoint, 0) FROM audit_prune()").Scan(&shadowed); err != nil || shadowed != 4 {
		t.Errorf("pruning with a temporary retention first in the search path: before %d, %v", shadowed, err)
	}
	_ = tx.Rollback(ctx)

	// With a retention longer than the events' age, nothing goes.
	if err := store.SetAuditRetention(ctx, owner, 3*365*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if p := prune(); p.Events != 0 {
		t.Errorf("pruned younger than the retention: %+v", p)
	}

	// With a year, what is before the old checkpoint goes, with its personal values.
	if err := store.SetAuditRetention(ctx, owner, 365*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if p := prune(); p.Checkpoint != 4 || p.Events != 3 {
		t.Errorf("pruned = %+v, want 3 events before 4", p)
	}
	if rows := all(t, s); rows[0].Seq != 4 || rows[len(rows)-1].Action != audit.ActionPruned {
		t.Errorf("after pruning, the log runs from %d to %s", rows[0].Seq, rows[len(rows)-1].Action)
	}
	if pd, err := s.AuditPersonalOf(ctx, []int64{2}); err != nil || len(pd) != 0 {
		t.Errorf("the personal values of pruned events: %v, %v", pd, err)
	}
	if cps, err := s.AuditCheckpoints(ctx); err != nil || len(cps) != 2 || cps[0].Seq != 4 {
		t.Errorf("checkpoints kept: %+v, %v", cps, err)
	}
	if p := prune(); p.Events != 0 {
		t.Errorf("pruned again: %+v", p)
	}
	res, err := audit.Verify(ctx, s.Pool, keys)
	if err != nil || res.Broken != nil || res.Start != 4 || res.First != 4 || res.Events != 7 || res.Checkpoints != 2 {
		t.Errorf("verify after pruning = %+v, %v, %v", res, res.Broken, err)
	}
	if res := verifyLines(t, keys, export(t, s)); res.Broken != nil || res.Start != 4 {
		t.Errorf("verify the export after pruning = %+v, %v", res, res.Broken)
	}
	if strings.Contains(strings.Join(export(t, s), "\n"), "recorded") {
		t.Error("the export holds when the database recorded the events")
	}

	// The same after recent events: one dated back to 2000 at the end of the chain and a
	// checkpoint of it, forged, then pruning with the shortest retention. Nothing goes, and
	// verification finds the forged checkpoint.
	if err := store.SetAuditRetention(ctx, owner, config.MinAuditRetention); err != nil {
		t.Fatal(err)
	}
	before := count()
	appendAt(t, s.Queries, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	last := all(t, s)[before]
	if err := s.InsertAuditCheckpoint(ctx, db.InsertAuditCheckpointParams{Seq: last.Seq, Hash: last.Hash, At: last.At, Key: "k", Signature: "s"}); err != nil {
		t.Fatal(err)
	}
	if p := prune(); p.Events != 0 || count() != before+1 {
		t.Errorf("pruned before an event the application dated back: %+v, %d of %d left", p, count(), before+1)
	}
	if res, err := audit.Verify(ctx, s.Pool, keys); err != nil || res.Broken == nil || res.Broken.Seq != last.Seq || !strings.Contains(res.Broken.Reason, "the checkpoint does not hold") {
		t.Errorf("verify with the forged checkpoint = %+v, %v, %v", res, res.Broken, err)
	}

	// Events deleted up to a checkpoint, but not by pruning: no audit.pruned event says so.
	if _, err := owner.Exec(ctx, `DELETE FROM audit_events WHERE seq < 8; DELETE FROM audit_checkpoints WHERE seq < 8`); err != nil {
		t.Fatal(err)
	}
	if res, err := audit.Verify(ctx, s.Pool, keys); err != nil || res.Broken == nil || res.Broken.Seq != 8 || !strings.Contains(res.Broken.Reason, "no audit.pruned event") {
		t.Errorf("verify after deleting before a checkpoint = %+v, %v, %v", res, res.Broken, err)
	}
}
