// Package repositories holds Quai's repository connections (docs/design/01-repository-connections.md):
// the Repository documents, stored in PostgreSQL or read from configuration files, and for each
// connection its mirror and a snapshot of the default branch's head with the definitions read
// from it. A snapshot is reloaded only when the head moves; reads fetch the mirror at most once
// per sync interval, and an explicit sync fetches at once. The registry reloads connections that
// change, without a restart.
package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/generic"
	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// ErrNotFound is returned for a repository that is not connected.
var ErrNotFound = errors.New("no such repository")

// Who manages a repository connection.
const (
	ManagedByConfig = "config"
	ManagedByAPI    = "api"
)

// ReloadInterval is how often a watching registry looks for connections that changed.
const ReloadInterval = 10 * time.Second

// Repo is one connected repository.
type Repo struct {
	Name       string
	Title      string
	Branch     string
	Delivery   string
	FloorRules []definitions.Rule
	Mirror     *gitx.Mirror
	Connector  connectors.Connector // the platform, as spec.connector names it
	// Document is the connection as declared; Generation moves when it changes.
	Document   Document
	Generation int64
	ManagedBy  string

	minInterval time.Duration
	secrets     *secrets.Store
	store       *store.Store // definition sets and events; nil without a database
	log         *slog.Logger
	mu          sync.Mutex
	lastFetch   time.Time
	snap        *Snapshot
	previews    map[string]*Snapshot // by commit
}

// Snapshot is a repository at one commit: its files and the definitions read from them.
type Snapshot struct {
	Repository  string
	Commit      string
	Definitions *definitions.Repository // the active ones: valid at Commit, or kept from before
	Problems    definitions.Problems
	Set         DefinitionSet // the active definitions' versions
	FS          fs.FS
	worktree    *gitx.Worktree
}

// Stored is a connection as its source holds it.
type Stored struct {
	Document   Document
	ManagedBy  string
	Generation int64
}

// Source lists the connections: the store, or configuration files when there is no database.
type Source func(ctx context.Context) ([]Stored, error)

// FromStore lists the connections stored in PostgreSQL.
func FromStore(s *store.Store) Source {
	return func(ctx context.Context) ([]Stored, error) {
		rows, err := s.ListRepositories(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]Stored, 0, len(rows))
		for _, r := range rows {
			var d Document
			if err := json.Unmarshal(r.Document, &d); err != nil {
				return nil, fmt.Errorf("repository %s: %w", r.Name, err)
			}
			out = append(out, Stored{Document: d, ManagedBy: r.ManagedBy, Generation: r.Generation})
		}
		return out, nil
	}
}

// Static lists fixed documents, as configuration-managed connections that never change.
func Static(docs []Document) Source {
	return func(context.Context) ([]Stored, error) {
		out := make([]Stored, len(docs))
		for i, d := range docs {
			out[i] = Stored{Document: d, ManagedBy: ManagedByConfig, Generation: 1}
		}
		return out, nil
	}
}

// Reconcile makes the connections managed by configuration those docs declare: it adds and
// updates them, and removes those docs no longer declare. A document named like a connection
// managed through the API is an error. Processes starting together reconcile one at a time.
func Reconcile(ctx context.Context, s *store.Store, docs []Document) error {
	return s.Tx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		if err := q.LockRepositories(ctx); err != nil {
			return err
		}
		rows, err := q.ListRepositories(ctx)
		if err != nil {
			return err
		}
		keep := make([]string, 0, len(docs))
		for _, d := range docs {
			name := d.Metadata.Name
			if i := slices.IndexFunc(rows, func(r db.ListRepositoriesRow) bool { return r.Name == name }); i >= 0 && rows[i].ManagedBy == ManagedByAPI {
				return fmt.Errorf("repository %q is managed through the API: delete it there, or give the one in the configuration another name", name)
			}
			doc, err := json.Marshal(d)
			if err != nil {
				return err
			}
			if err := q.PutConfigRepository(ctx, db.PutConfigRepositoryParams{Name: name, Document: doc}); err != nil {
				return err
			}
			keep = append(keep, name)
		}
		_, err = q.DeleteConfigRepositoriesExcept(ctx, keep)
		return err
	})
}

// Registry is the set of connected repositories.
type Registry struct {
	src        Source
	connectors *connectors.Registry
	secrets    *secrets.Store
	db         *store.Store
	cache      string
	committer  gitx.Signature
	signing    func(context.Context) ([]byte, error) // the commits' signing key; nil: unsigned
	caFile     string                                // the CA bundle git verifies servers with; empty: git's own
	proxy      func(*url.URL) (*url.URL, error)
	log        *slog.Logger

	mu    sync.RWMutex
	repos map[string]*Repo
	// failed holds the generations that could not be opened. They are retried on every reload,
	// and logged as errors the first time only.
	failed map[string]int64
}

// worktreeGrace is how long a replaced snapshot's files stay readable for requests still using
// it.
const worktreeGrace = 2 * time.Minute

// Options are what a registry works with besides its source.
type Options struct {
	Secrets    *secrets.Store       // resolves the connections' secret references
	Network    *netx.Network        // how git reaches the remotes; nil: git's defaults, no proxy
	Connectors *connectors.Registry // makes each connection's connector; nil: Git transport only
	// Store keeps the definitions active at each commit, and their changes as events; nil
	// without a database.
	Store *store.Store
	Log   *slog.Logger
}

// Open loads the connections of src: it makes their connectors, clones or fetches their mirrors
// and compiles their floor rules. A connection that cannot be opened (an unreachable remote,
// say) is logged and left out until a reload opens it.
func Open(ctx context.Context, cfg *config.Config, src Source, opts Options) (*Registry, error) {
	store, network, log := opts.Secrets, opts.Network, opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	conns := opts.Connectors
	if conns == nil {
		conns = &connectors.Registry{Factories: map[string]connectors.Factory{"generic": generic.New}, Fallback: generic.Fallback, Log: log}
	}
	var err error
	cache := cfg.Git.CacheDir
	if cache == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		cache = filepath.Join(dir, "quai", "mirrors")
	}
	reg := &Registry{src: src, connectors: conns, secrets: store, db: opts.Store, cache: cache, log: log, repos: map[string]*Repo{}, failed: map[string]int64{},
		committer: gitx.Signature{Name: cfg.Git.Committer.Name, Email: cfg.Git.Committer.Email}}
	if ref := cfg.Git.SigningKey.Ref; ref != "" && store != nil {
		reg.signing = func(ctx context.Context) ([]byte, error) {
			key, err := store.Resolve(ctx, ref)
			return []byte(key.Reveal()), err
		}
	}
	if network != nil {
		reg.proxy = network.Proxy
		if bundle := network.GitBundle(); bundle != nil {
			if reg.caFile, err = writeBundle(cache, bundle); err != nil {
				return nil, err
			}
		}
	}
	if err := reg.Reload(ctx); err != nil {
		return nil, err
	}
	return reg, nil
}

// writeBundle writes the CA bundle git uses into the cache directory, replacing it whole.
func writeBundle(cache string, bundle []byte) (string, error) {
	if err := os.MkdirAll(cache, 0o750); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(cache, "ca-bundle-*.pem")
	if err != nil {
		return "", err
	}
	_, err = f.Write(bundle)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	path := filepath.Join(cache, "ca-bundle.pem")
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("CA bundle for git: %w", err)
	}
	return path, nil
}

// Reload brings the registry in line with its source: it opens new connections, reopens those
// whose generation moved, and drops those that are gone. A connection that fails to reopen keeps
// its previous version.
func (r *Registry) Reload(ctx context.Context) error {
	stored, err := r.src(ctx)
	if err != nil {
		return err
	}
	r.mu.RLock()
	current, failed := maps.Clone(r.repos), maps.Clone(r.failed)
	r.mu.RUnlock()

	next, nextFailed := map[string]*Repo{}, map[string]int64{}
	for _, s := range stored {
		name := s.Document.Metadata.Name
		prev := current[name]
		if prev != nil && prev.Generation == s.Generation {
			next[name] = prev
			continue
		}
		repo, err := r.open(ctx, s, prev)
		if err != nil {
			level := slog.LevelError
			if g, ok := failed[name]; ok && g == s.Generation {
				level = slog.LevelDebug
			}
			r.log.Log(ctx, level, "repository not connected", slog.String("repository", name), slog.Any("error", err))
			nextFailed[name] = s.Generation
			if prev != nil {
				next[name] = prev
			}
			continue
		}
		next[name] = repo
		if prev != nil {
			r.log.InfoContext(ctx, "repository reloaded", slog.String("repository", name), slog.Int64("generation", s.Generation))
		}
	}
	r.mu.Lock()
	old := r.repos
	r.repos, r.failed = next, nextFailed
	r.mu.Unlock()
	for name, repo := range old {
		if next[name] == nil {
			r.log.InfoContext(ctx, "repository disconnected", slog.String("repository", name))
			repo.closeLater()
		}
	}
	return nil
}

// open connects one repository, reusing prev's mirror, and its snapshot when the branch stays,
// when the URL does not change.
func (r *Registry) open(ctx context.Context, s Stored, prev *Repo) (*Repo, error) {
	d := s.Document
	env, err := expr.NewEnv()
	if err != nil {
		return nil, err
	}
	repo := &Repo{Name: d.Metadata.Name, Title: d.Metadata.Title, Branch: d.Spec.DefaultBranch, Delivery: d.Spec.Delivery.Mode,
		Document: d, Generation: s.Generation, ManagedBy: s.ManagedBy, minInterval: time.Duration(d.Spec.Sync.MinInterval),
		secrets: r.secrets, store: r.db, log: r.log, lastFetch: time.Now()}
	repo.checkCredentials(ctx)
	if repo.Connector, err = r.connectors.New(d.Spec.Connector, connectors.Config{URL: d.Spec.URL, Credentials: repo.Credentials,
		KnownHosts: d.Spec.Credentials.KnownHosts}); err != nil {
		return nil, err
	}
	for _, f := range d.Spec.Enforcement.FloorRules {
		rule := definitions.Rule{Name: f.Name, When: f.When, Approvers: f.Approvers, Min: f.Min}
		if f.When != "" {
			if rule.Condition, err = env.CompileCondition(f.When); err != nil {
				return nil, fmt.Errorf("floor rule %s: %w", f.Name, err)
			}
		}
		repo.FloorRules = append(repo.FloorRules, rule)
	}
	// The mirror is kept while git reaches the remote the same way.
	if prev != nil && prev.Document.Spec.URL == d.Spec.URL && prev.Document.Spec.Connector == d.Spec.Connector &&
		prev.Document.Spec.Credentials == d.Spec.Credentials {
		prev.mu.Lock()
		repo.Mirror, repo.lastFetch = prev.Mirror, prev.lastFetch
		if prev.Branch == repo.Branch {
			repo.snap, prev.snap = prev.snap, nil
		}
		prev.mu.Unlock()
		return repo, nil
	}
	opts := gitx.Options{Committer: r.committer, Auth: repo.Connector.GitAuth, CAFile: r.caFile, Proxy: r.proxy,
		PartialClone: d.Spec.Sync.PartialClone, SigningKey: r.signing}
	if repo.Mirror, err = gitx.OpenMirror(ctx, d.Spec.URL, filepath.Join(r.cache, repo.Name+".git"), opts); err != nil {
		return nil, err
	}
	if prev != nil {
		prev.closeLater()
	}
	return repo, nil
}

// Watch reloads the registry every interval until ctx ends; errors are logged.
func (r *Registry) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Reload(ctx); err != nil && ctx.Err() == nil {
				r.log.ErrorContext(ctx, "reloading the repositories", slog.Any("error", err))
			}
		}
	}
}

// List returns the connected repositories by name.
func (r *Registry) List() []*Repo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := slices.Collect(maps.Values(r.repos))
	slices.SortFunc(out, func(a, b *Repo) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Get returns a repository by name.
func (r *Registry) Get(name string) (*Repo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if repo := r.repos[name]; repo != nil {
		return repo, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// Close removes the snapshots' worktrees.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, repo := range r.repos {
		repo.mu.Lock()
		if repo.snap != nil {
			_ = repo.snap.worktree.Close()
			repo.snap = nil
		}
		repo.mu.Unlock()
	}
}

// ExpiryWarning is how long before a credential expires Quai starts warning.
const ExpiryWarning = 30 * 24 * time.Hour

// Credentials resolves the connection's credentials (spec.credentials.ref); ok is false when it
// names none.
func (repo *Repo) Credentials(ctx context.Context) (s secrets.Secret, ok bool, err error) {
	return repo.resolve(ctx, repo.Document.Spec.Credentials.Ref)
}

// WebhookSecret resolves the secret the platform's webhooks carry (spec.webhooks.secret.ref).
func (repo *Repo) WebhookSecret(ctx context.Context) (s secrets.Secret, ok bool, err error) {
	return repo.resolve(ctx, repo.Document.Spec.Webhooks.Secret.Ref)
}

// RunToken resolves the token runners report with (spec.runner.reportToken.ref); ok is false
// when the repository takes no run reports.
func (repo *Repo) RunToken(ctx context.Context) (s secrets.Secret, ok bool, err error) {
	return repo.resolve(ctx, repo.Document.Spec.Runner.ReportToken.Ref)
}

func (repo *Repo) resolve(ctx context.Context, ref string) (secrets.Secret, bool, error) {
	if ref == "" {
		return secrets.Secret{}, false, nil
	}
	if repo.secrets == nil {
		return secrets.Secret{}, false, fmt.Errorf("repository %s: no secret store", repo.Name)
	}
	s, err := repo.secrets.Resolve(ctx, ref)
	if err != nil {
		return secrets.Secret{}, false, fmt.Errorf("repository %s: %w", repo.Name, err)
	}
	return s, true, nil
}

// checkCredentials warns when the connection's credentials cannot be read, or expire soon.
func (repo *Repo) checkCredentials(ctx context.Context) {
	s, ok, err := repo.Credentials(ctx)
	switch {
	case err != nil:
		repo.log.WarnContext(ctx, "credentials cannot be read", slog.String("repository", repo.Name), slog.Any("error", err))
	case ok && s.ExpiresWithin(ExpiryWarning, time.Now()):
		repo.log.WarnContext(ctx, "credentials expire soon", slog.String("repository", repo.Name), slog.Time("expires", s.Expires))
	}
}

// closeLater removes the snapshot's worktree once requests still reading it are done.
func (repo *Repo) closeLater() {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if old := repo.snap; old != nil {
		repo.snap = nil
		time.AfterFunc(worktreeGrace, func() { _ = old.worktree.Close() })
	}
}

// Refresh fetches the mirror unless it was fetched at t or later: what another process committed
// at t, such as a worker delivering a request, is then in the snapshots that follow.
func (repo *Repo) Refresh(ctx context.Context, t time.Time) error {
	repo.mu.Lock()
	fresh := !repo.lastFetch.Before(t)
	repo.mu.Unlock()
	if fresh {
		return nil
	}
	_, err := repo.Snapshot(ctx, true)
	return err
}

// Snapshot returns the repository at the head of its branch, fetching first when the last fetch
// is older than the sync interval, or always when sync is set.
func (repo *Repo) Snapshot(ctx context.Context, sync bool) (*Snapshot, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if sync || time.Since(repo.lastFetch) >= repo.minInterval {
		if err := repo.Mirror.Fetch(ctx); err != nil {
			if repo.snap == nil {
				return nil, err
			}
			// Serve the last snapshot while the remote is unreachable.
			repo.log.WarnContext(ctx, "fetch failed; serving the last snapshot", slog.String("repository", repo.Name), slog.Any("error", err))
		} else {
			repo.lastFetch = time.Now()
		}
	}
	head, err := repo.Mirror.Head(ctx, repo.Branch)
	if err != nil {
		return nil, err
	}
	if repo.snap != nil && repo.snap.Commit == head {
		return repo.snap, nil
	}
	wt, err := repo.Mirror.Worktree(ctx, head)
	if err != nil {
		return nil, err
	}
	defs, problems := definitions.Load(wt.FS())
	set := repo.activate(ctx, head, defs, problems, wt.FS())
	snap := &Snapshot{Repository: repo.Name, Commit: head, Definitions: defs, Problems: problems, Set: set, FS: wt.FS(), worktree: wt}
	if old := repo.snap; old != nil {
		time.AfterFunc(worktreeGrace, func() { _ = old.worktree.Close() })
	}
	repo.snap = snap
	repo.log.InfoContext(ctx, "definitions loaded", slog.String("repository", repo.Name), slog.String("commit", head),
		slog.Int("types", len(defs.Types)), slog.Int("blueprints", len(defs.Blueprints)), slog.Int("problems", len(problems)))
	return snap, nil
}
