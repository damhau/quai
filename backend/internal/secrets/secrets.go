// Package secrets resolves secret references (docs/design/01-repository-connections.md#credentials).
// Quai stores references only, never secret values (docs/design/09-security-and-compliance.md):
// `env:NAME` reads Quai's environment, `k8s:namespace/name#key` a Kubernetes Secret. Values are
// read when they are used, kept for a short while, and never logged nor returned by the API: a
// Secret prints as [redacted].
package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when a reference names a secret, or a key, that does not exist.
var ErrNotFound = errors.New("secret not found")

const redacted = "[redacted]"

// Secret is a secret value. Printing, logging or encoding it shows [redacted]; Reveal gives the
// value to the code that sends it.
type Secret struct {
	value string
	// Expires is when the secret stops working, if its source says (zero otherwise).
	Expires time.Time
}

// FromValue makes a Secret of a value Quai has in hand: one it generated (a webhook secret, a
// run token), or a test's.
func FromValue(value string) Secret { return Secret{value: value} }

// Reveal returns the secret's value.
func (s Secret) Reveal() string { return s.value }

func (s Secret) String() string               { return redacted }
func (s Secret) GoString() string             { return redacted }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// ExpiresWithin reports whether the secret is known to expire before now+d.
func (s Secret) ExpiresWithin(d time.Duration, now time.Time) bool {
	return !s.Expires.IsZero() && s.Expires.Before(now.Add(d))
}

// Backend reads the secrets of one kind of reference, given the reference without its prefix.
type Backend interface {
	Get(ctx context.Context, ref string) (Secret, error)
}

// refRe is the shape of a reference: a scheme, a colon and a non-empty name.
var refRe = regexp.MustCompile(`^([a-z0-9]+):(\S+)$`)

// Store resolves references through its backends, keeping values for a while so that a busy
// server does not read the same secret on every request, and picks up rotations within that time.
type Store struct {
	backends map[string]Backend
	ttl      time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	secret Secret
	at     time.Time
}

// TTL is how long a Store keeps a value it read.
const TTL = time.Minute

// New returns a store over backends, by scheme ("env", "k8s").
func New(backends map[string]Backend) *Store {
	return &Store{backends: backends, ttl: TTL, now: time.Now, cache: map[string]cached{}}
}

// Resolve returns the secret ref names.
func (s *Store) Resolve(ctx context.Context, ref string) (Secret, error) {
	m := refRe.FindStringSubmatch(ref)
	if m == nil {
		return Secret{}, fmt.Errorf("%q is not a secret reference such as env:NAME or k8s:namespace/name#key", ref)
	}
	b := s.backends[m[1]]
	if b == nil {
		return Secret{}, fmt.Errorf("%s: no %s secret store is configured", ref, m[1])
	}
	now := s.now()
	s.mu.Lock()
	c, ok := s.cache[ref]
	s.mu.Unlock()
	if ok && now.Sub(c.at) < s.ttl {
		return c.secret, nil
	}
	sec, err := b.Get(ctx, m[2])
	if err != nil {
		return Secret{}, fmt.Errorf("%s: %w", ref, err)
	}
	s.mu.Lock()
	s.cache[ref] = cached{secret: sec, at: now}
	s.mu.Unlock()
	return sec, nil
}

// Env reads `env:NAME` from Quai's environment.
type Env struct {
	Lookup func(string) (string, bool) // os.LookupEnv in production
}

func (e Env) Get(_ context.Context, name string) (Secret, error) {
	v, ok := e.Lookup(name)
	if !ok || v == "" {
		return Secret{}, fmt.Errorf("%w: the environment variable is not set", ErrNotFound)
	}
	return Secret{value: v}, nil
}

// splitK8s splits namespace/name#key; the key is optional.
func splitK8s(ref string) (namespace, name, key string, err error) {
	path, key, _ := strings.Cut(ref, "#")
	namespace, name, ok := strings.Cut(path, "/")
	if !ok || namespace == "" || name == "" || strings.Contains(name, "/") {
		return "", "", "", errors.New("a Kubernetes reference is k8s:namespace/name, optionally followed by #key")
	}
	return namespace, name, key, nil
}
