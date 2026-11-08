package keeper_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/audit/keeper"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/connectors"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/giteatest"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Around(func() int { return giteatest.Main(m) })()) }

var ctx = context.Background()

// syncBuffer is a log the keeper writes to while the test reads it.
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
	reg     *repositories.Registry
	gitea   *giteatest.Instance
	remote  *giteatest.Repo
	secrets *secrets.Store
	vars    map[string]string
	now     time.Time
	log     *syncBuffer
	base    int64 // the last seq before the test's events
}

// newKey generates a signing key in PEM, as the secret store holds it, and its public key.
func newKey(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), pub
}

// setup connects an audit repository on Gitea, audit-log with direct delivery, and infra with
// pull requests; the signing key is env:AUDIT_KEY.
func setup(t *testing.T) *env {
	t.Helper()
	g := giteatest.Get(t)
	r, other := g.Repo(t, "audit"), g.Repo(t, "infra")
	s := pgtest.Store(t)
	key, _ := newKey(t)
	e := &env{t: t, store: s, gitea: g, remote: r, vars: map[string]string{"GITEA_TOKEN": g.Token, "AUDIT_KEY": key}, now: time.Now(), log: &syncBuffer{}}
	e.secrets = secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := e.vars[n]; return v, ok }}})
	docs, err := repositories.Parse([]byte(`apiVersion: quai.dev/v1beta1
kind: Repository
metadata: { name: audit-log }
spec:
  connector: gitea
  url: ` + r.CloneURL + `
  credentials: { ref: env:GITEA_TOKEN }
  delivery: { mode: direct }
---
apiVersion: quai.dev/v1beta1
kind: Repository
metadata: { name: infra }
spec:
  connector: gitea
  url: ` + other.CloneURL + `
  credentials: { ref: env:GITEA_TOKEN }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := repositories.Reconcile(ctx, s, docs); err != nil {
		t.Fatal(err)
	}
	gitCfg := config.Git{CacheDir: t.TempDir(), Committer: config.Committer{Name: "Quai", Email: "quai@localhost"}}
	e.reg, err = repositories.Open(ctx, &config.Config{Git: gitCfg}, repositories.FromStore(s), repositories.Options{
		Secrets: e.secrets, Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"gitea": giteaconn.New}},
		Store: s, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.reg.Close)
	// The connections' repository.created events.
	last, err := s.LastAuditEvent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.base = last.Seq
	return e
}

// keeper returns a keeper of the environment, configured by cfg.
func (e *env) keeper(cfg config.Audit) *keeper.Keeper {
	return keeper.New(keeper.Options{Store: e.store, Config: cfg, Secrets: e.secrets, Repositories: e.reg,
		Committer: gitx.Signature{Name: "Quai", Email: "quai@localhost"}, Log: slog.New(slog.NewTextHandler(e.log, nil)),
		Now: func() time.Time { return e.now }})
}

func signing(repository string, previous ...string) config.Audit {
	c := config.Audit{Retention: 8760 * time.Hour}
	c.Checkpoints.SigningKey.Ref = "env:AUDIT_KEY"
	c.Checkpoints.Repository = repository
	c.Checkpoints.PreviousKeys = previous
	return c
}

func (e *env) record(n int) {
	e.t.Helper()
	for i := range n {
		if err := e.store.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			return audit.Add(ctx, tx, audit.Event{Action: "test.event", Object: audit.Object{Type: "test", ID: "x"}, Details: map[string]any{"n": i}})
		}); err != nil {
			e.t.Fatal(err)
		}
	}
}

// committed returns the checkpoint the audit repository holds for c.
func (e *env) committed(c audit.Checkpoint) audit.Checkpoint {
	e.t.Helper()
	file, err := keeper.File(c)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _, err := e.gitea.Client.GetFile(e.remote.Owner, e.remote.Name, e.remote.DefaultBranch, file)
	if err != nil {
		e.t.Fatalf("%s in the audit repository: %v", file, err)
	}
	var got audit.Checkpoint
	if err := json.Unmarshal(b, &got); err != nil {
		e.t.Fatal(err)
	}
	return got
}

// lastVerification returns the last audit.verified or audit.broken event's action and details.
func (e *env) lastVerification() (string, map[string]any) {
	e.t.Helper()
	ev, err := e.store.LastAuditVerification(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	var d map[string]any
	_ = json.Unmarshal(ev.Details, &d)
	return ev.Action, d
}

// TestCheckpoints: each checkpoint is signed and committed to the audit repository; the daily
// verification checks the log against the latest, and finds a database restored from an older
// backup.
func TestCheckpoints(t *testing.T) {
	e := setup(t)
	k := e.keeper(signing("audit-log"))

	if a, err := k.Anchor(ctx); err != nil || len(a) != 0 {
		t.Errorf("anchor of an empty audit repository = %+v, %v", a, err)
	}
	e.record(3)
	c3, err := k.Checkpoint(ctx)
	if err != nil || c3.Seq != e.base+3 {
		t.Fatalf("checkpoint = %+v, %v", c3, err)
	}
	if got := e.committed(c3); got != c3 {
		t.Errorf("committed %+v, signed %+v", got, c3)
	}
	if again, err := k.Checkpoint(ctx); err != nil || again != c3 {
		t.Errorf("checkpoint with no new event = %+v, %v", again, err)
	}
	e.record(2)
	if err := k.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	c5, err := e.store.LatestAuditCheckpoint(ctx)
	if err != nil || c5.Seq != e.base+5 || e.committed(audit.CheckpointOf(c5)).Seq != c5.Seq {
		t.Fatalf("checkpoint at shutdown = %+v, %v", c5, err)
	}
	if a, err := k.Anchor(ctx); err != nil || len(a) != 1 || a[0] != audit.CheckpointOf(c5) {
		t.Errorf("anchor = %+v, %v", a, err)
	}
	if !strings.Contains(e.log.String(), "audit checkpoint committed") {
		t.Errorf("log:\n%s", e.log)
	}

	// The daily verification.
	if due, err := k.Due(ctx); err != nil || !due {
		t.Errorf("due before any verification = %v, %v", due, err)
	}
	res, err := k.Verify(ctx)
	if err != nil || res.Broken != nil || res.Events != e.base+5 || res.Checkpoints != 2 {
		t.Errorf("verify = %+v, %v", res, err)
	}
	if action, d := e.lastVerification(); action != keeper.ActionVerified || d["events"] != float64(e.base+5) {
		t.Errorf("recorded %s %v", action, d)
	}
	if due, err := k.Due(ctx); err != nil || due {
		t.Errorf("due right after a verification = %v, %v", due, err)
	}
	e.now = time.Now().Add(keeper.VerifyEvery)
	if due, err := k.Due(ctx); err != nil || !due {
		t.Errorf("due a day later = %v, %v", due, err)
	}

	// The database restored from a backup taken at the first checkpoint: the audit repository has
	// the second.
	owner, err := pgxpool.New(ctx, pgtest.OwnerURL(e.store.Pool.Config().ConnString()))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.Exec(ctx, `DELETE FROM audit_events WHERE seq > $1`, c3.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `DELETE FROM audit_checkpoints WHERE seq > $1`, c3.Seq); err != nil {
		t.Fatal(err)
	}
	res, err = k.Verify(ctx)
	if err != nil || res.Broken == nil || res.Broken.Seq != c3.Seq+1 || !strings.Contains(res.Broken.Reason, fmt.Sprintf("checkpoint in the audit repository says it reached seq %d", c5.Seq)) {
		t.Errorf("verify after a restore = %+v, %v, %v", res, res.Broken, err)
	}
	if action, d := e.lastVerification(); action != keeper.ActionBroken || d["seq"] != float64(c3.Seq+1) {
		t.Errorf("recorded %s %v", action, d)
	}
	if !strings.Contains(e.log.String(), "level=ERROR msg=\"the audit log does not verify") {
		t.Errorf("log:\n%s", e.log)
	}

	// A checkpoint in the audit repository that no key signed.
	forged := audit.Checkpoint{Type: audit.CheckpointType, Seq: c5.Seq + 4, Hash: c5.Hash, At: audit.Time(e.now), Key: c3.Key, Signature: c3.Signature}
	b, _ := json.Marshal(forged)
	file, _ := keeper.File(forged)
	if err := e.remote.Commit(e.remote.DefaultBranch, file, string(b)); err != nil {
		t.Fatal(err)
	}
	if res, err := k.Verify(ctx); err != nil || res.Broken == nil || !strings.Contains(res.Broken.Reason, "its signature does not verify") {
		t.Errorf("verify with a forged checkpoint in the audit repository = %v, %v", res.Broken, err)
	}
}

// TestRotation: after the signing key changes, checkpoints of the previous one verify with its
// public key, and only with it.
func TestRotation(t *testing.T) {
	e := setup(t)
	e.record(2)
	if _, err := e.keeper(signing("")).Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	old, oldPub := e.vars["AUDIT_KEY"], ed25519.PublicKey(nil)
	if k, err := audit.ParsePrivateKey([]byte(old)); err == nil {
		oldPub = k.Public().(ed25519.PublicKey)
	}
	pubPEM, err := audit.MarshalPublicKey(oldPub)
	if err != nil {
		t.Fatal(err)
	}
	e.vars["AUDIT_KEY"], _ = newKey(t)
	// The secret store caches values for a minute: a new store reads the new key.
	e.secrets = secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := e.vars[n]; return v, ok }}})
	e.record(1)
	if c, err := e.keeper(signing("")).Checkpoint(ctx); err != nil || c.Seq != e.base+3 {
		t.Fatalf("checkpoint with the new key = %+v, %v", c, err)
	}

	if res, err := e.keeper(signing("")).Verify(ctx); err != nil || res.Broken == nil || res.Broken.Seq != e.base+2 || !strings.Contains(res.Broken.Reason, "which was not given") {
		t.Errorf("verify without the previous key = %+v, %v, %v", res, res.Broken, err)
	}
	if res, err := e.keeper(signing("", string(pubPEM))).Verify(ctx); err != nil || res.Broken != nil || res.Checkpoints != 2 {
		t.Errorf("verify with the previous key = %+v, %v, %v", res, res.Broken, err)
	}

	// Without a signing key: no checkpoint, and the previous keys still verify.
	none := config.Audit{Retention: 8760 * time.Hour}
	if c, err := e.keeper(none).Checkpoint(ctx); err != nil || c.Seq != 0 {
		t.Errorf("checkpoint without a key = %+v, %v", c, err)
	}
	if res, err := e.keeper(none).Verify(ctx); err != nil || res.Broken == nil || res.Broken.Seq != e.base+2 {
		t.Errorf("verify with no key = %+v, %v", res.Broken, err)
	}
}

// TestAuditRepository: the audit repository must exist and take direct commits; a checkpoint it
// did not take stays in the log, and verification does not pass without it.
func TestAuditRepository(t *testing.T) {
	e := setup(t)
	e.record(1)
	for name, want := range map[string]string{
		"infra":   "the audit repository infra must have delivery mode direct, not pull-request",
		"missing": "no such repository: missing",
	} {
		k := e.keeper(signing(name))
		if c, err := k.Checkpoint(ctx); err == nil || !strings.Contains(err.Error(), want) || c.Seq != e.base+1 {
			t.Errorf("%s: checkpoint = %+v, %v", name, c, err)
		}
		if res, err := k.Verify(ctx); err == nil || !strings.Contains(err.Error(), want) || res.Broken != nil {
			t.Errorf("%s: verify = %+v, %v", name, res, err)
		}
	}
	if _, err := e.store.LastAuditVerification(ctx); err != pgx.ErrNoRows {
		t.Errorf("a verification without the audit repository was recorded: %v", err)
	}
	e.vars["AUDIT_KEY"] = "not a key"
	e.secrets = secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := e.vars[n]; return v, ok }}})
	if _, err := e.keeper(signing("")).Checkpoint(ctx); err == nil || strings.Contains(err.Error(), "not a key") {
		t.Errorf("checkpoint with a malformed key: %v", err)
	}
}
