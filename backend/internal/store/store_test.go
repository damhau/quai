package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var ctx = context.Background()

func newRequest(target string) db.CreateRequestParams {
	return db.CreateRequestParams{
		ID: uuid.New(), Kind: "action", Action: "resize", Target: target, TypeName: "vm",
		Requester: "bob", Reason: "load", Inputs: json.RawMessage(`{"cpu": 8}`),
		State: "pending_approval", DefinitionsCommit: "c0ffee", BaseCommit: "c0ffee",
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	s := pgtest.Store(t)
	n, err := store.Migrate(ctx, s.Pool)
	if err != nil || n != 0 {
		t.Errorf("Migrate on a migrated database = %d, %v", n, err)
	}
	if _, err := store.Open(ctx, "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"); err == nil {
		t.Error("Open of an unreachable database succeeded")
	}
	if _, err := store.Open(ctx, "::not a url"); err == nil {
		t.Error("Open of a bad URL succeeded")
	}
}

func TestRequests(t *testing.T) {
	s := pgtest.Store(t)
	p := newRequest("infra-live/vm/prod/web01")
	r, err := s.CreateRequest(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != p.ID || r.State != "pending_approval" || !r.CreatedAt.Valid || string(r.Inputs) != `{"cpu": 8}` {
		t.Errorf("created = %+v", r)
	}
	// One open request per target.
	var pgErr *pgconn.PgError
	if _, err := s.CreateRequest(ctx, newRequest(p.Target)); !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("a second open request: %v", err)
	}
	// Transitions only from the expected state.
	if _, err := s.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: "approved", ToState: "merging"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a transition from the wrong state: %v", err)
	}
	done, err := s.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: "pending_approval", ToState: "applied"})
	if err != nil || done.State != "applied" {
		t.Fatalf("transition = %+v, %v", done, err)
	}
	// Once it is closed, a new request for the target is allowed.
	next, err := s.CreateRequest(ctx, newRequest(p.Target))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRequest(ctx, db.CreateRequestParams{ID: uuid.New(), Kind: "rename", Target: "x", Requester: "bob", State: "planned", Inputs: json.RawMessage(`{}`)}); err == nil {
		t.Error("an unknown kind was stored")
	}

	all, err := s.ListRequests(ctx, db.ListRequestsParams{States: []string{}, MaxRows: 10})
	if err != nil || len(all) != 2 || all[0].ID != next.ID {
		t.Errorf("all = %d, %v", len(all), err)
	}
	open, err := s.ListRequests(ctx, db.ListRequestsParams{States: []string{"pending_approval"}, Requester: "bob", Target: p.Target, MaxRows: 10})
	if err != nil || len(open) != 1 || open[0].ID != next.ID {
		t.Errorf("open = %+v, %v", open, err)
	}
	if got, err := s.GetRequest(ctx, r.ID); err != nil || got.State != "applied" {
		t.Errorf("GetRequest = %+v, %v", got, err)
	}
}

func TestChangeSetsReviewsEvents(t *testing.T) {
	s := pgtest.Store(t)
	r, err := s.CreateRequest(ctx, newRequest("infra-live/vm/prod/web01"))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := s.CreateChangeSet(ctx, db.CreateChangeSetParams{ID: uuid.New(), RequestID: r.ID, Repository: "infra-live", Hash: "sha256:ab", Fingerprints: json.RawMessage(`{"envs/prod/vms/web01.yaml": "sha256:cd"}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []db.CreateChangeParams{
		{ChangeSetID: cs.ID, Path: "envs/prod/vms/web01.yaml", Op: "update", Before: []byte("cpu: 4\n"), After: []byte("cpu: 8\n")},
		{ChangeSetID: cs.ID, Path: "envs/prod/vms/old.yaml", Op: "delete", Before: []byte("x\n")},
	} {
		if err := s.CreateChange(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	changes, err := s.ListChanges(ctx, cs.ID)
	if err != nil || len(changes) != 2 || changes[0].Path != "envs/prod/vms/old.yaml" || changes[0].After != nil || string(changes[1].After) != "cpu: 8\n" {
		t.Errorf("changes = %+v, %v", changes, err)
	}
	if sets, err := s.ListChangeSets(ctx, r.ID); err != nil || len(sets) != 1 || sets[0].Hash != "sha256:ab" {
		t.Errorf("change sets = %+v, %v", sets, err)
	}

	for i, d := range []string{"approve", "deny"} {
		if _, err := s.CreateReview(ctx, db.CreateReviewParams{ID: uuid.New(), RequestID: r.ID, Reviewer: []string{"alice", "fiona"}[i], Groups: []string{"g"}, Decision: d, ChangeSetHash: cs.Hash}); err != nil {
			t.Fatal(err)
		}
	}
	reviews, err := s.ListReviews(ctx, r.ID)
	if err != nil || len(reviews) != 2 || reviews[0].Reviewer != "alice" || reviews[1].Decision != "deny" {
		t.Errorf("reviews = %+v, %v", reviews, err)
	}

	for _, kind := range []string{"submitted", "approved"} {
		if _, err := s.AddEvent(ctx, db.AddEventParams{RequestID: pgtype.UUID{Bytes: r.ID, Valid: true}, Target: r.Target, Kind: kind, Actor: "bob", Data: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddEvent(ctx, db.AddEventParams{Target: r.Target, Kind: "drifted", Data: json.RawMessage(`{"stack": "envs/prod"}`)}); err != nil {
		t.Fatal(err)
	}
	if evs, err := s.ListRequestEvents(ctx, pgtype.UUID{Bytes: r.ID, Valid: true}); err != nil || len(evs) != 2 || evs[0].Kind != "submitted" {
		t.Errorf("request events = %+v, %v", evs, err)
	}
	if evs, err := s.ListTargetEvents(ctx, db.ListTargetEventsParams{Target: r.Target, MaxRows: 2}); err != nil || len(evs) != 2 || evs[0].Kind != "drifted" {
		t.Errorf("target events = %+v, %v", evs, err)
	}
}

func TestRuns(t *testing.T) {
	s := pgtest.Store(t)
	run, err := s.StartRun(ctx, db.StartRunParams{ID: uuid.New(), Repository: "infra-live", Stack: "envs/prod", Commit: "abc", Kind: "apply", Url: "https://ci/1"})
	if err != nil || run.Status != "running" {
		t.Fatalf("StartRun = %+v, %v", run, err)
	}
	done, err := s.FinishRun(ctx, db.FinishRunParams{ID: run.ID, Status: "succeeded", Summary: json.RawMessage(`{"changed": 1}`)})
	if err != nil || done.Status != "succeeded" || !done.FinishedAt.Valid {
		t.Errorf("FinishRun = %+v, %v", done, err)
	}
	if _, err := s.FinishRun(ctx, db.FinishRunParams{ID: run.ID, Status: "failed", Summary: json.RawMessage(`{}`)}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("finishing a finished run: %v", err)
	}
	latest, err := s.LatestRun(ctx, db.LatestRunParams{Repository: "infra-live", Stack: "envs/prod", Kind: "apply"})
	if err != nil || latest.ID != run.ID {
		t.Errorf("LatestRun = %+v, %v", latest, err)
	}
}

func TestAPITokens(t *testing.T) {
	s := pgtest.Store(t)
	hash := sha256.Sum256([]byte("quai_secret"))
	tok, err := s.CreateAPIToken(ctx, db.CreateAPITokenParams{
		ID: uuid.New(), Owner: "bob", Name: "ci", Hash: hash[:], Scopes: []string{"requests:write"},
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetActiveAPIToken(ctx, hash[:])
	if err != nil || got.ID != tok.ID || got.Scopes[0] != "requests:write" {
		t.Fatalf("GetActiveAPIToken = %+v, %v", got, err)
	}
	if err := s.TouchAPIToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RevokeAPIToken(ctx, db.RevokeAPITokenParams{ID: tok.ID, Owner: "mallory"}); err != nil || n != 0 {
		t.Errorf("revoke by someone else = %d, %v", n, err)
	}
	if n, err := s.RevokeAPIToken(ctx, db.RevokeAPITokenParams{ID: tok.ID, Owner: "bob"}); err != nil || n != 1 {
		t.Errorf("revoke = %d, %v", n, err)
	}
	if _, err := s.GetActiveAPIToken(ctx, hash[:]); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a revoked token is active: %v", err)
	}
	expired := sha256.Sum256([]byte("old"))
	if _, err := s.CreateAPIToken(ctx, db.CreateAPITokenParams{ID: uuid.New(), Owner: "bob", Name: "old", Hash: expired[:], Scopes: []string{}, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetActiveAPIToken(ctx, expired[:]); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("an expired token is active: %v", err)
	}
	if list, err := s.ListAPITokens(ctx, "bob"); err != nil || len(list) != 2 || !list[0].LastUsedAt.Valid || !list[0].RevokedAt.Valid {
		t.Errorf("ListAPITokens = %+v, %v", list, err)
	}
}

func TestTx(t *testing.T) {
	s := pgtest.Store(t)
	boom := errors.New("boom")
	err := s.Tx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := q.CreateRequest(ctx, newRequest("a")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx = %v", err)
	}
	if n, _ := s.ListRequests(ctx, db.ListRequestsParams{States: []string{}, MaxRows: 10}); len(n) != 0 {
		t.Error("a rolled back request was stored")
	}
	if err := s.Tx(ctx, func(_ pgx.Tx, q *db.Queries) error { _, err := q.CreateRequest(ctx, newRequest("b")); return err }); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.ListRequests(ctx, db.ListRequestsParams{States: []string{}, MaxRows: 10}); len(n) != 1 {
		t.Error("a committed request is missing")
	}
}
