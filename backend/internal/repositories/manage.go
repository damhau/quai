package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Managing connections through the API (docs/design/01-repository-connections.md#repository-connection):
// those declared in configuration are read-only there, and a connection's name never changes.

var (
	// ErrExists is returned when creating a connection whose name is taken.
	ErrExists = errors.New("a repository with this name is already connected")
	// ErrReadOnly is returned when changing a connection declared in configuration.
	ErrReadOnly = errors.New("the repository is declared in Quai's configuration: change it there")
	// ErrNoDatabase is returned when connections can only be declared in configuration.
	ErrNoDatabase = errors.New("without a database, repositories are declared in configuration")
)

// Connection is a stored connection: its document, and whether this process connected it.
type Connection struct {
	Stored
	Repo *Repo // nil while not connected
	Err  error // why it could not be connected, if it was tried
}

// Connection returns the stored connection name, connected or not.
func (r *Registry) Connection(ctx context.Context, name string) (Connection, error) {
	stored, err := r.src(ctx)
	if err != nil {
		return Connection{}, err
	}
	i := slices.IndexFunc(stored, func(s Stored) bool { return s.Document.Metadata.Name == name })
	if i < 0 {
		return Connection{}, ErrNotFound
	}
	c := Connection{Stored: stored[i]}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if repo := r.repos[name]; repo != nil && repo.Generation == c.Generation {
		c.Repo = repo
	}
	if f, ok := r.failed[name]; ok && f.generation == c.Generation {
		c.Err = f.err
	}
	return c, nil
}

// Create stores a new connection, managed through the API, and connects it. A connection that
// cannot be opened is stored all the same, with the reason (Connection.Err), and retried.
func (r *Registry) Create(ctx context.Context, d Document) (Connection, error) {
	if r.db == nil {
		return Connection{}, ErrNoDatabase
	}
	d.ApplyDefaults()
	if err := d.Validate(); err != nil {
		return Connection{}, err
	}
	doc, err := json.Marshal(d)
	if err != nil {
		return Connection{}, err
	}
	var pgErr *pgconn.PgError
	if err := r.db.CreateRepository(ctx, db.CreateRepositoryParams{Name: d.Metadata.Name, Document: doc}); errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Connection{}, ErrExists
	} else if err != nil {
		return Connection{}, err
	}
	if err := r.Reload(ctx); err != nil {
		return Connection{}, err
	}
	return r.Connection(ctx, d.Metadata.Name)
}

// Update replaces a connection managed through the API; its name cannot change.
func (r *Registry) Update(ctx context.Context, name string, d Document) (Connection, error) {
	if r.db == nil {
		return Connection{}, ErrNoDatabase
	}
	if d.Metadata.Name != name {
		return Connection{}, &InvalidError{Name: name, Problems: []string{"metadata.name cannot change: it is part of every resource ID"}}
	}
	d.ApplyDefaults()
	if err := d.Validate(); err != nil {
		return Connection{}, err
	}
	doc, err := json.Marshal(d)
	if err != nil {
		return Connection{}, err
	}
	n, err := r.db.UpdateRepository(ctx, db.UpdateRepositoryParams{Name: name, Document: doc})
	if err != nil {
		return Connection{}, err
	}
	if n == 0 {
		return Connection{}, r.unchangeable(ctx, name)
	}
	if err := r.Reload(ctx); err != nil {
		return Connection{}, err
	}
	return r.Connection(ctx, name)
}

// Delete removes a connection managed through the API. Its requests and history stay.
func (r *Registry) Delete(ctx context.Context, name string) error {
	if r.db == nil {
		return ErrNoDatabase
	}
	n, err := r.db.DeleteRepository(ctx, name)
	if err != nil {
		return err
	}
	if n == 0 {
		return r.unchangeable(ctx, name)
	}
	return r.Reload(ctx)
}

// unchangeable says why the API could not change name: it is declared in configuration, or it
// does not exist.
func (r *Registry) unchangeable(ctx context.Context, name string) error {
	c, err := r.Connection(ctx, name)
	if err != nil {
		return err
	}
	if c.ManagedBy == ManagedByConfig {
		return ErrReadOnly
	}
	return ErrNotFound
}

// ErrNoWebhookSecret is returned when installing the webhook of a connection without
// spec.webhooks.secret.
var ErrNoWebhookSecret = errors.New("the connection has no webhook secret: set spec.webhooks.secret.ref first")

// InstallWebhook installs, or updates, the platform's webhook delivering the repository's events
// to url, with its secret. connectors.ErrUnsupported when the connector cannot.
func (repo *Repo) InstallWebhook(ctx context.Context, url string) error {
	hooks := repo.Connector.Webhooks()
	if hooks == nil {
		return fmt.Errorf("%w: the %s connector installs no webhooks", connectors.ErrUnsupported, repo.Document.Spec.Connector)
	}
	secret, ok, err := repo.WebhookSecret(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoWebhookSecret
	}
	return hooks.Subscribe(ctx, url, secret)
}
