// Package config loads Quai's configuration: a YAML file, overridden by QUAI_* environment
// variables, validated at start-up (docs/operations/deployment-and-operations.md). It is the
// only package that reads the environment; everything else receives a *Config.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Environments.
const (
	Development = "development"
	Production  = "production"
)

// Config is Quai's configuration.
type Config struct {
	// Env is development or production. Development-only features, such as the user switcher,
	// are refused in production.
	Env          string       `yaml:"env"`
	Server       Server       `yaml:"server"`
	Database     Database     `yaml:"database"`
	Auth         Auth         `yaml:"auth"`
	Git          Git          `yaml:"git"`
	Repositories Repositories `yaml:"repositories"`
	Secrets      Secrets      `yaml:"secrets"`
	Log          Log          `yaml:"log"`
	Telemetry    Telemetry    `yaml:"telemetry"`
}

// Server configures the HTTP server.
type Server struct {
	Addr            string        `yaml:"addr"`            // listen address, such as :8080
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"` // how long requests in flight may finish
}

// Database configures PostgreSQL.
type Database struct {
	URL string `yaml:"url"` // such as postgres://quai@db:5432/quai; secrets belong in QUAI_DATABASE_URL
}

// Git configures how Quai works with repositories.
type Git struct {
	CacheDir  string    `yaml:"cacheDir"` // where repository mirrors live; the user cache directory by default
	Committer Committer `yaml:"committer"`
}

// Committer is the identity Quai commits as; authors are the requesters.
type Committer struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

// Repositories says where the Repository documents managed by the configuration are
// (docs/design/01-repository-connections.md): a YAML file, or a directory of them. They are
// reconciled into the database at start-up; the API manages the others.
type Repositories struct {
	Path string `yaml:"path"`
}

// Secrets configures where secret references resolve (docs/design/01-repository-connections.md#credentials):
// `env:NAME` from Quai's environment, `k8s:namespace/name#key` from Kubernetes Secrets.
type Secrets struct {
	Kubernetes struct {
		// Kubeconfig is the file to reach the cluster with; empty: the cluster Quai runs in, or
		// the default kubeconfig (KUBECONFIG, ~/.kube/config) outside one.
		Kubeconfig string `yaml:"kubeconfig"`
	} `yaml:"kubernetes"`
}

// UnmarshalYAML refuses the list of repositories of earlier versions with a pointer to where its
// entries went.
func (r *Repositories) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.SequenceNode {
		return errors.New("repositories is now { path: <file or directory> } holding Repository documents " +
			"(apiVersion: quai.dev/v1beta1, kind: Repository); see docs/design/01-repository-connections.md")
	}
	type plain Repositories
	return n.Decode((*plain)(r))
}

// Authentication modes.
const (
	AuthOIDC = "oidc"
	AuthDev  = "dev"
)

// Auth configures who can sign in and the roles their groups give them.
type Auth struct {
	// Mode is oidc, or dev for the development user switcher, which production refuses.
	Mode    string        `yaml:"mode"`
	OIDC    OIDC          `yaml:"oidc"`
	Roles   []RoleMapping `yaml:"roles"`
	Session Session       `yaml:"session"`
	Dev     Dev           `yaml:"dev"`
}

// OIDC configures login with an OpenID Connect provider (authorization code with PKCE).
type OIDC struct {
	Issuer       string   `yaml:"issuer"`
	ClientID     string   `yaml:"clientID"`
	ClientSecret string   `yaml:"clientSecret"` // better in QUAI_AUTH_OIDC_CLIENT_SECRET
	RedirectURL  string   `yaml:"redirectURL"`  // https://quai.example.com/auth/callback
	Scopes       []string `yaml:"scopes"`       // openid, profile, email by default
	NameClaim    string   `yaml:"nameClaim"`    // preferred_username by default
	GroupsClaim  string   `yaml:"groupsClaim"`  // groups by default; roles for Entra app roles
}

// RoleMapping grants a role to the members of an IdP group at a scope: global (the default),
// team:<name>, repository:<name> or catalog-item:<name>.
type RoleMapping struct {
	Group string `yaml:"group"`
	Role  string `yaml:"role"`
	Scope string `yaml:"scope"`
}

// Session configures browser sessions.
type Session struct {
	Lifetime    time.Duration `yaml:"lifetime"`    // 8h by default
	IdleTimeout time.Duration `yaml:"idleTimeout"` // 30m by default
}

// Dev lists the users of the development switcher.
type Dev struct {
	Users []DevUser `yaml:"users"`
}

// DevUser is a user of the development switcher.
type DevUser struct {
	Name   string   `yaml:"name"`
	Groups []string `yaml:"groups"`
}

// Log configures logging.
type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn or error
	Format string `yaml:"format"` // json or text; json in production by default
}

// Telemetry configures OpenTelemetry. Without an endpoint, nothing is exported.
type Telemetry struct {
	OTLPEndpoint string `yaml:"otlpEndpoint"` // the collector's base URL, such as http://otel-collector:4318
	ServiceName  string `yaml:"serviceName"`
}

// variables maps each environment variable to the setting it overrides.
var variables = []struct {
	name string
	set  func(*Config, string) error
}{
	{"QUAI_ENV", func(c *Config, v string) error { c.Env = v; return nil }},
	{"QUAI_SERVER_ADDR", func(c *Config, v string) error { c.Server.Addr = v; return nil }},
	{"QUAI_SERVER_SHUTDOWN_TIMEOUT", func(c *Config, v string) (err error) {
		c.Server.ShutdownTimeout, err = time.ParseDuration(v)
		return err
	}},
	{"QUAI_DATABASE_URL", func(c *Config, v string) error { c.Database.URL = v; return nil }},
	{"QUAI_AUTH_MODE", func(c *Config, v string) error { c.Auth.Mode = v; return nil }},
	{"QUAI_AUTH_OIDC_ISSUER", func(c *Config, v string) error { c.Auth.OIDC.Issuer = v; return nil }},
	{"QUAI_AUTH_OIDC_CLIENT_ID", func(c *Config, v string) error { c.Auth.OIDC.ClientID = v; return nil }},
	{"QUAI_AUTH_OIDC_CLIENT_SECRET", func(c *Config, v string) error { c.Auth.OIDC.ClientSecret = v; return nil }},
	{"QUAI_AUTH_OIDC_REDIRECT_URL", func(c *Config, v string) error { c.Auth.OIDC.RedirectURL = v; return nil }},
	{"QUAI_GIT_CACHE_DIR", func(c *Config, v string) error { c.Git.CacheDir = v; return nil }},
	{"QUAI_LOG_LEVEL", func(c *Config, v string) error { c.Log.Level = v; return nil }},
	{"QUAI_LOG_FORMAT", func(c *Config, v string) error { c.Log.Format = v; return nil }},
	{"QUAI_TELEMETRY_OTLP_ENDPOINT", func(c *Config, v string) error { c.Telemetry.OTLPEndpoint = v; return nil }},
	{"QUAI_TELEMETRY_SERVICE_NAME", func(c *Config, v string) error { c.Telemetry.ServiceName = v; return nil }},
	{"QUAI_REPOSITORIES_PATH", func(c *Config, v string) error { c.Repositories.Path = v; return nil }},
	{"QUAI_SECRETS_KUBECONFIG", func(c *Config, v string) error { c.Secrets.Kubernetes.Kubeconfig = v; return nil }},
}

// Load reads the configuration file at path, or at QUAI_CONFIG when path is empty, applies the
// environment variables that lookup finds (os.LookupEnv in production), fills in defaults and
// validates the result. Without a file, the configuration comes from the environment and the
// defaults. Unknown keys in the file are errors.
func Load(path string, lookup func(string) (string, bool)) (*Config, error) {
	c := &Config{}
	if path == "" {
		path, _ = lookup("QUAI_CONFIG")
	}
	if path != "" {
		b, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's --config flag
		if err != nil {
			return nil, fmt.Errorf("configuration: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("configuration %s: %w", path, err)
		}
	}
	for _, v := range variables {
		if s, ok := lookup(v.name); ok {
			if err := v.set(c, s); err != nil {
				return nil, fmt.Errorf("configuration: %s: %w", v.name, err)
			}
		}
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	return c, nil
}

func (c *Config) defaults() {
	if c.Env == "" {
		c.Env = Development
	}
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = 20 * time.Second
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
		if c.Env == Production {
			c.Log.Format = "json"
		}
	}
	if c.Telemetry.ServiceName == "" {
		c.Telemetry.ServiceName = "quai"
	}
	if c.Git.Committer.Name == "" {
		c.Git.Committer = Committer{Name: "Quai", Email: "quai@localhost"}
	}
	a := &c.Auth
	if a.Mode == "" {
		a.Mode = AuthOIDC
		if c.Env == Development {
			a.Mode = AuthDev
		}
	}
	if len(a.OIDC.Scopes) == 0 {
		a.OIDC.Scopes = []string{"openid", "profile", "email"}
	}
	if a.OIDC.NameClaim == "" {
		a.OIDC.NameClaim = "preferred_username"
	}
	if a.OIDC.GroupsClaim == "" {
		a.OIDC.GroupsClaim = "groups"
	}
	if a.Session.Lifetime == 0 {
		a.Session.Lifetime = 8 * time.Hour
	}
	if a.Session.IdleTimeout == 0 {
		a.Session.IdleTimeout = 30 * time.Minute
	}
	if a.Mode == AuthDev && len(a.Dev.Users) == 0 && len(a.Roles) == 0 {
		// The prototype's demo users, with roles to try every screen.
		a.Dev.Users = []DevUser{
			{Name: "alice", Groups: []string{"platform-admins"}},
			{Name: "bob", Groups: []string{"developers", "shop"}},
			{Name: "fiona", Groups: []string{"finops", "developers"}},
			{Name: "carol", Groups: []string{"shop", "developers"}},
		}
		a.Roles = []RoleMapping{
			{Group: "platform-admins", Role: "platform-admin"},
			{Group: "developers", Role: "viewer"},
			{Group: "developers", Role: "requester"},
			{Group: "developers", Role: "operator"},
			{Group: "finops", Role: "viewer"},
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	if c.Env != Development && c.Env != Production {
		errs = append(errs, fmt.Errorf("env must be %s or %s, not %q", Development, Production, c.Env))
	}
	if _, port, err := net.SplitHostPort(c.Server.Addr); err != nil || port == "" {
		errs = append(errs, fmt.Errorf("server.addr must be host:port or :port, not %q", c.Server.Addr))
	}
	if u := c.Database.URL; u != "" {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "postgres" && p.Scheme != "postgresql") {
			errs = append(errs, errors.New("database.url must be a postgres:// URL"))
		}
	}
	if c.Server.ShutdownTimeout < 0 {
		errs = append(errs, errors.New("server.shutdownTimeout must not be negative"))
	}
	errs = append(errs, c.Auth.validate(c.Env))
	if _, err := c.Log.SlogLevel(); err != nil {
		errs = append(errs, err)
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		errs = append(errs, fmt.Errorf("log.format must be json or text, not %q", c.Log.Format))
	}
	if e := c.Telemetry.OTLPEndpoint; e != "" {
		u, err := url.Parse(e)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("telemetry.otlpEndpoint must be an http or https URL, not %q", e))
		}
	}
	return errors.Join(errs...)
}

// roles are the role names of docs/design/08; internal/authz defines them.
var roles = []string{"viewer", "requester", "operator", "catalog-author", "repository-admin", "platform-admin", "auditor", "break-glass"}

func (a *Auth) validate(env string) error {
	var errs []error
	switch a.Mode {
	case AuthDev:
		if env == Production {
			errs = append(errs, errors.New("auth.mode dev (the user switcher) is refused in production"))
		}
		for _, u := range a.Dev.Users {
			if u.Name == "" {
				errs = append(errs, errors.New("auth.dev.users: every user needs a name"))
			}
		}
	case AuthOIDC:
		o := a.OIDC
		if u, err := url.Parse(o.Issuer); err != nil || u.Host == "" || (u.Scheme != "https" && (env == Production || u.Scheme != "http")) {
			errs = append(errs, errors.New("auth.oidc.issuer must be an https URL (http only in development)"))
		}
		if o.ClientID == "" {
			errs = append(errs, errors.New("auth.oidc.clientID is required"))
		}
		if u, err := url.Parse(o.RedirectURL); err != nil || u.Host == "" || (u.Scheme != "https" && (env == Production || u.Scheme != "http")) {
			errs = append(errs, errors.New("auth.oidc.redirectURL must be an https URL (http only in development)"))
		}
	default:
		errs = append(errs, fmt.Errorf("auth.mode must be oidc or dev, not %q", a.Mode))
	}
	for i, m := range a.Roles {
		if m.Group == "" {
			errs = append(errs, fmt.Errorf("auth.roles[%d].group is required", i))
		}
		if !slices.Contains(roles, m.Role) {
			errs = append(errs, fmt.Errorf("auth.roles[%d].role must be one of %s, not %q", i, strings.Join(roles, ", "), m.Role))
		}
		if kind, name, ok := strings.Cut(m.Scope, ":"); m.Scope != "" && m.Scope != "global" && (!ok || name == "" || !slices.Contains([]string{"team", "repository", "catalog-item"}, kind)) {
			errs = append(errs, fmt.Errorf("auth.roles[%d].scope must be global, team:<name>, repository:<name> or catalog-item:<name>, not %q", i, m.Scope))
		}
	}
	if a.Session.Lifetime < 0 || a.Session.IdleTimeout < 0 {
		errs = append(errs, errors.New("auth.session durations must not be negative"))
	}
	return errors.Join(errs...)
}

// SlogLevel returns the log level.
func (l Log) SlogLevel() (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(l.Level))); err != nil {
		return 0, fmt.Errorf("log.level must be debug, info, warn or error, not %q", l.Level)
	}
	return level, nil
}

// Logger returns the logger the configuration describes, writing to w.
func (c *Config) Logger(w io.Writer) *slog.Logger {
	level, _ := c.Log.SlogLevel()
	opts := &slog.HandlerOptions{Level: level}
	if c.Log.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
