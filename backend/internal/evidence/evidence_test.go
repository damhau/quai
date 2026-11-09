package evidence_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/evidence"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var ctx = context.Background()

const (
	requestID = "0b9d5a52-6c1e-4f43-9d0e-6f1f1c1a2b3c"
	commit    = "c0ffee1234567890c0ffee1234567890c0ffee12"
	defs      = "d3f5d3f5d3f5d3f5d3f5d3f5d3f5d3f5d3f5d3f5"
)

var (
	bobAuth   = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	aliceAuth = time.Date(2026, 11, 2, 10, 30, 0, 0, time.UTC)
	bob       = audit.Actor{Type: audit.User, ID: "Bob Builder", IP: "10.0.0.2", AuthTime: bobAuth}
	alice     = audit.Actor{Type: audit.User, ID: "Alice Admin", IP: "10.0.0.1", AuthTime: aliceAuth}
	runner    = audit.Actor{Type: audit.Runner, ID: "infra-live"}
	change    = plan.Change{Path: "envs/prod/vms/web01.yaml", Op: plan.OpUpdate, Before: []byte("name: web01\ncpu: 4\nmemory: 8\n"), After: []byte("name: web01\ncpu: 8\nmemory: 8\n")}
)

func newKey(t testing.TB) (ed25519.PrivateKey, audit.Keys) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := audit.Keys{}
	keys.Add(pub)
	return key, keys
}

func record(t testing.TB, s *store.Store, events ...audit.Event) {
	t.Helper()
	if err := s.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error { return audit.Add(ctx, tx, events...) }); err != nil {
		t.Fatal(err)
	}
}

func sign(t testing.TB, s *store.Store, key ed25519.PrivateKey) audit.Checkpoint {
	t.Helper()
	c, _, err := audit.SignLatest(ctx, s.Queries, key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func on(id string, actor audit.Actor, action string, details map[string]any) audit.Event {
	return audit.Event{Actor: actor, Action: action, Object: audit.Object{Type: "request", ID: id, Resource: "infra-live/vm/prod/web01"}, Details: details}
}

// noise is what others do meanwhile: people sign in, other requests are submitted.
func noise(n int) []audit.Event {
	var out []audit.Event
	for i := range n {
		person := fmt.Sprintf("Person %d", i%37)
		if i%3 == 0 {
			out = append(out, audit.Event{Actor: audit.Actor{Type: audit.User, ID: person, IP: fmt.Sprintf("10.1.%d.%d", i%250, i%200), AuthTime: bobAuth},
				Action: "auth.login", Object: audit.Object{Type: "user", ID: person}, Details: map[string]any{"acr": "urn:mfa"}})
			continue
		}
		other := fmt.Sprintf("6f1f1c1a-0000-4000-8000-%012d", i)
		out = append(out, on(other, audit.Actor{Type: audit.User, ID: person, IP: "10.2.0.1", AuthTime: bobAuth}, "request.submitted", map[string]any{
			"kind": "update", "type": "vm", "repository": "infra-live", "changeSet": "sha256:" + strings.Repeat("ab", 32),
			"definitionsCommit": defs, "reason": audit.Personal("Someone else's reason, which a bundle shows only as a pseudonym."),
			"expiry": "", "separationOfDuties": "requester-cannot-approve"}))
	}
	return out
}

// history records a request applied after a review: two checkpoints prove all but the last event.
func history(t testing.TB, s *store.Store, key ed25519.PrivateKey, between int) {
	t.Helper()
	cs := plan.Hash([]plan.Change{change})
	record(t, s, noise(3)...)
	record(t, s, on(requestID, bob, "request.submitted", map[string]any{"kind": "update", "type": "vm", "repository": "infra-live", "changeSet": cs,
		"definitionsCommit": defs, "reason": audit.Personal("Black Friday traffic."), "expiry": "", "separationOfDuties": "requester-cannot-approve"}))
	record(t, s, noise(between)...)
	sign(t, s, key)
	record(t, s, noise(2)...)
	record(t, s, on(requestID, alice, "request.reviewed", map[string]any{"decision": "approve", "comment": audit.Personal("Fine for the sale."), "changeSet": cs,
		"stepUp": true, "onBehalfOf": audit.Personal(""), "delegation": "", "acr": "urn:mfa"}),
		on(requestID, audit.Quai, "request.approved", map[string]any{"changeSet": cs, "approvals": []audit.Personal{"Alice Admin"}}))
	record(t, s, noise(between)...)
	record(t, s, on(requestID, audit.Quai, "request.merged", map[string]any{"from": "approved", "commit": commit, "pullRequest": "#7"}))
	record(t, s, noise(2)...)
	sign(t, s, key)
	record(t, s, on(requestID, runner, "request.applied", map[string]any{"from": "applying", "event": "apply_succeeded", "commit": commit, "run": "https://ci/run/1"}))
	record(t, s, noise(1)...)
}

// bundle makes the request's bundle: the summary as the API makes it from the request's rows.
func bundle(t testing.TB, s *store.Store, omitted string) *evidence.Bundle {
	t.Helper()
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := evidence.Prove(ctx, db.New(tx), requestID, omitted)
	if err != nil {
		t.Fatal(err)
	}
	cs := plan.Hash([]plan.Change{change})
	return &evidence.Bundle{
		BundleVersion: evidence.Version, GeneratedAt: "2026-11-02T12:00:00.000Z", Quai: "test",
		Request: evidence.Request{ID: requestID, Number: 42, Kind: "update", Repository: "infra-live", Target: "infra-live/vm/prod/web01", Type: "vm",
			Requester: "Bob Builder", Reason: "Black Friday traffic.", State: "applied", Inputs: json.RawMessage(`{"cpu":8}`), CreatedAt: "2026-11-02T09:01:00.000Z",
			UpdatedAt: "2026-11-02T11:00:00.000Z", DefinitionsCommit: defs, Owners: []string{"team:shop"}, Rules: []evidence.Rule{}},
		ChangeSets: []evidence.ChangeSet{{Repository: "infra-live", Hash: cs, Commit: commit, Changes: []evidence.Change{evidence.NewChange(change)}}},
		Plans:      []evidence.Run{},
		Approvals: []evidence.Approval{{Reviewer: "Alice Admin", Groups: []string{"platform-admins"}, Decision: "approve", Comment: "Fine for the sale.", ChangeSet: cs,
			At: "2026-11-02T10:31:00.000Z", AuthTime: aliceAuth.Format(time.RFC3339), ACR: "urn:mfa", StepUp: true}},
		Delivery: &evidence.Delivery{Branch: "quai/42", PullRequest: "#7", MergeCommit: commit, Trailers: []string{"Quai-Request: #42 0b9d5a52", "Approved-by: Alice Admin"}},
		Applies:  []evidence.Run{{Stack: "envs/prod", Commit: commit, Status: "succeeded", StartedAt: "2026-11-02T11:00:00.000Z", Policies: []evidence.Policy{}}},
		Events:   a.Events, Personal: a.Personal, Proof: a.Proof, Notes: []string{},
	}
}

// roundTrip writes a bundle and reads it back, as an auditor's verifier does.
func roundTrip(t testing.TB, b *evidence.Bundle) *evidence.Bundle {
	t.Helper()
	data, err := evidence.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	again, err := evidence.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return again
}

func TestBundleVerifies(t *testing.T) {
	s := pgtest.Store(t)
	key, keys := newKey(t)
	history(t, s, key, 20)
	b := bundle(t, s, "")

	if len(b.Events) != 5 || len(b.Proof.Segments) != 2 {
		t.Fatalf("events = %d, segments = %d", len(b.Events), len(b.Proof.Segments))
	}
	// The segments hold others' events as the log has them: no clear personal value of theirs.
	data, err := evidence.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"Someone else's reason", "Person 1", "10.1.", "10.0.0.1", "10.0.0.2"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Errorf("the bundle discloses %q", leak)
		}
	}
	for _, clear := range []string{"Black Friday traffic.", "Alice Admin", "Fine for the sale.", "Bob Builder"} {
		if !bytes.Contains(data, []byte(clear)) {
			t.Errorf("the bundle lacks the request's own %q", clear)
		}
	}
	// The same bundle, the same bytes.
	if again, _ := evidence.Marshal(roundTrip(t, b)); !bytes.Equal(again, data) {
		t.Error("a bundle read back is not written the same")
	}

	r := evidence.Verify(roundTrip(t, b), keys)
	if !r.OK() || r.Events != 5 || r.Proven != 4 || len(r.Checkpoints) != 2 || len(r.Unproven) != 1 || r.Unproven[0].Reason != evidence.NotYet {
		t.Fatalf("report = %+v", r)
	}
	// After the next checkpoint, everything is proven.
	sign(t, s, key)
	if r := evidence.Verify(roundTrip(t, bundle(t, s, "")), keys); !r.OK() || r.Proven != 5 || len(r.Unproven) != 0 {
		t.Errorf("report after the next checkpoint = %+v", r)
	}
	// Another key does not verify it.
	_, other := newKey(t)
	if r := evidence.Verify(b, other); r.OK() || !strings.Contains(strings.Join(r.Failures, "\n"), "checkpoint") {
		t.Errorf("verified with another key: %+v", r)
	}
}

// TestEditedBundleFails: each edit of a bundle fails, with a message saying what.
func TestEditedBundleFails(t *testing.T) {
	s := pgtest.Store(t)
	key, keys := newKey(t)
	history(t, s, key, 20)
	sign(t, s, key)
	other := sign2(t, s, key)
	for _, c := range []struct {
		name string
		edit func(b *evidence.Bundle)
		want string
	}{
		{"one byte of an event", func(b *evidence.Bundle) {
			b.Events[0].Details = bytes.Replace(b.Events[0].Details, []byte(`"vm"`), []byte(`"wm"`), 1)
		}, "event seq"},
		{"one byte of another event in a segment", func(b *evidence.Bundle) {
			r := &b.Proof.Segments[0].Records[1]
			r.At = strings.Replace(r.At, "Z", "+00:00", 1)
		}, "the record was changed"},
		{"an event removed", func(b *evidence.Bundle) { b.Events = append(b.Events[:1], b.Events[2:]...) }, "an event was removed"},
		{"an event moved out of the request", func(b *evidence.Bundle) {
			seg := &b.Proof.Segments[1]
			seg.Records = append(seg.Records, b.Events[1])
			b.Events = append(b.Events[:1], b.Events[2:]...)
		}, "leave out"},
		{"a checkpoint swapped", func(b *evidence.Bundle) { b.Proof.Segments[0].Checkpoint = other }, "checkpoint"},
		{"a checkpoint's signature", func(b *evidence.Bundle) {
			c := &b.Proof.Segments[1].Checkpoint
			flip := "A"
			if c.Signature[10] == 'A' {
				flip = "B"
			}
			c.Signature = c.Signature[:10] + flip + c.Signature[11:]
		}, "checkpoint"},
		{"the state", func(b *evidence.Bundle) { b.Request.State = "rejected" }, "state"},
		{"the reason", func(b *evidence.Bundle) { b.Request.Reason = "A routine change." }, "reason"},
		{"the requester", func(b *evidence.Bundle) { b.Request.Requester = "Mallory" }, "requester"},
		{"the reviewer", func(b *evidence.Bundle) { b.Approvals[0].Reviewer = "Carol Checker" }, "review by Carol Checker"},
		{"an authentication time", func(b *evidence.Bundle) { b.Approvals[0].AuthTime = "2026-11-02T10:00:00Z" }, "review by"},
		{"a review's comment", func(b *evidence.Bundle) { b.Approvals[0].Comment = "Not reviewed." }, "comment"},
		{"a review added", func(b *evidence.Bundle) { b.Approvals = append(b.Approvals, b.Approvals[0]) }, "reviews"},
		{"the merge commit", func(b *evidence.Bundle) { b.Delivery.MergeCommit = strings.Repeat("1", 40) }, "merge commit"},
		{"a change's content", func(b *evidence.Bundle) {
			b.ChangeSets[0].Changes[0].After = strings.Replace(b.ChangeSets[0].Changes[0].After, "8", "16", 1)
		}, "afterSha256"},
		{"a diff", func(b *evidence.Bundle) { b.ChangeSets[0].Changes[0].Diff[1] = "+cpu: 2" }, "its diff is not the diff"},
		{"a clear personal value", func(b *evidence.Bundle) {
			for k, v := range b.Personal {
				if v.Value == "Alice Admin" {
					v.Value = "Carol Checker"
					b.Personal[k] = v
				}
			}
		}, "not the one its pseudonym was made of"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := roundTrip(t, bundle(t, s, ""))
			if r := evidence.Verify(b, keys); !r.OK() {
				t.Fatalf("the bundle fails before the edit: %v", r.Failures)
			}
			c.edit(b)
			r := evidence.Verify(roundTrip(t, b), keys)
			if r.OK() || !strings.Contains(strings.Join(r.Failures, "\n"), c.want) {
				t.Errorf("failures = %q, want one saying %q", r.Failures, c.want)
			}
		})
	}
}

// sign2 records an event and signs a checkpoint of it: a valid checkpoint of another place.
func sign2(t testing.TB, s *store.Store, key ed25519.PrivateKey) audit.Checkpoint {
	t.Helper()
	record(t, s, noise(1)...)
	return sign(t, s, key)
}

// TestUnproven: without a checkpoint nothing is proven, and the bundle says why; a token's
// bundle has no proof.
func TestUnproven(t *testing.T) {
	s := pgtest.Store(t)
	_, keys := newKey(t)
	record(t, s, on(requestID, bob, "request.submitted", map[string]any{"kind": "update", "type": "vm", "repository": "infra-live",
		"changeSet": plan.Hash([]plan.Change{change}), "definitionsCommit": defs, "reason": audit.Personal("Black Friday traffic.")}))
	b := bundle(t, s, "")
	b.Request.State, b.Approvals, b.Delivery, b.ChangeSets[0].Commit = "pending_approval", []evidence.Approval{}, nil, ""
	r := evidence.Verify(roundTrip(t, b), keys)
	if !r.OK() || r.Proven != 0 || len(r.Unproven) != 1 || r.Unproven[0].Reason != evidence.NoCheckpoint {
		t.Errorf("report without a checkpoint = %+v", r)
	}
	b = bundle(t, s, "a bundle for a token carries no proof")
	if b.Proof.Omitted == "" || len(b.Proof.Segments) != 0 || len(b.Proof.Unproven) != 1 {
		t.Errorf("proof omitted = %+v", b.Proof)
	}
}

// TestProofBound: a segment beyond the bound is left out, its events unproven; the bundle of a
// realistic busy log stays small. The size is written to docs/design/09-security-and-compliance.md.
func TestProofBound(t *testing.T) {
	if testing.Short() {
		t.Skip("records over 5000 events")
	}
	s := pgtest.Store(t)
	key, keys := newKey(t)
	// A busy instance: 600 events an hour between the request's events, a checkpoint each hour.
	history(t, s, key, 600)
	sign(t, s, key)
	b := bundle(t, s, "")
	data, _ := evidence.Marshal(b)
	records := 0
	for _, seg := range b.Proof.Segments {
		records += len(seg.Records)
	}
	t.Logf("a bundle proving 5 events with %d records of 600-event hours: %d KiB, %d bytes a record", records, len(data)>>10, len(data)/max(1, records))
	if r := evidence.Verify(roundTrip(t, b), keys); !r.OK() || r.Proven != 5 {
		t.Errorf("report = %+v", r)
	}
	// Beyond the bound: an event whose next checkpoint is more than 5000 records away.
	record(t, s, on(requestID, alice, "request.reminded", map[string]any{}))
	record(t, s, noise(evidence.MaxProofRecords)...)
	sign(t, s, key)
	b = bundle(t, s, "")
	r := evidence.Verify(roundTrip(t, b), keys)
	if !r.OK() || r.Proven != 5 || len(r.Unproven) != 1 || !strings.Contains(r.Unproven[0].Reason, "beyond the bundle's bound") {
		t.Errorf("report beyond the bound = %+v", r)
	}
}

// FuzzParse: Parse and Verify never panic, whatever the bytes.
func FuzzParse(f *testing.F) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	keys := audit.Keys{}
	keys.Add(key.Public().(ed25519.PublicKey))
	r := audit.Record{Seq: 1, At: "2026-11-02T09:00:00.000Z", Actor: json.RawMessage(`{"type":"system","id":"quai"}`), Action: "request.submitted",
		Object: json.RawMessage(`{"type":"request","id":"` + requestID + `"}`), Details: json.RawMessage(`{"kind":"update"}`), Prev: audit.Genesis}
	r.Hash, _ = r.Digest()
	c, _ := audit.Sign(key, 1, r.Hash, time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC))
	b := &evidence.Bundle{BundleVersion: evidence.Version, Request: evidence.Request{ID: requestID, Kind: "update"}, Events: []audit.Record{r},
		ChangeSets: []evidence.ChangeSet{{Hash: plan.Hash([]plan.Change{change}), Changes: []evidence.Change{evidence.NewChange(change)}}},
		Proof:      evidence.Proof{Segments: []evidence.Segment{{From: 1, To: 1, Checkpoint: c, Records: []audit.Record{}}}}}
	seed, _ := evidence.Marshal(b)
	f.Add(seed)
	f.Add([]byte(`{"bundleVersion":1}`))
	f.Add([]byte(`{"bundleVersion":1,"proof":{"segments":[{"from":1,"to":9000000000000000000}]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := evidence.Parse(bytes.NewReader(data))
		if err != nil {
			return
		}
		_ = evidence.Verify(b, keys)
		if _, err := evidence.PDF(b); err != nil {
			t.Errorf("PDF of a bundle that parses: %v", err)
		}
	})
}
