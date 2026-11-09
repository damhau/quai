// Package config loads Quai's configuration: a YAML file, overridden by QUAI_* environment
// variables, validated at start-up (docs/operations/deployment-and-operations.md). It is the
// only package that reads the environment; everything else receives a *Config.
package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // time zones even where the system has no database of them (TimeZone)

	"github.com/google/uuid"
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
	Env           string        `yaml:"env"`
	Server        Server        `yaml:"server"`
	Database      Database      `yaml:"database"`
	Auth          Auth          `yaml:"auth"`
	Git           Git           `yaml:"git"`
	Repositories  Repositories  `yaml:"repositories"`
	Secrets       Secrets       `yaml:"secrets"`
	Network       Network       `yaml:"network"`
	Webhooks      Webhooks      `yaml:"webhooks"`
	Audit         Audit         `yaml:"audit"`
	Notifications Notifications `yaml:"notifications"`
	Log           Log           `yaml:"log"`
	Telemetry     Telemetry     `yaml:"telemetry"`
}

// Notifications configures how Quai tells people what happens to requests, and whom it alerts
// (docs/design/05-delivery-and-approvals.md#notifications). Without a channel, nobody is
// notified.
type Notifications struct {
	// Email sends notifications by email, through an SMTP server; nil: no email.
	Email *Email `yaml:"email"`
	// Teams posts to Microsoft Teams channels through Workflows webhooks, and sends people
	// personal cards through a bot; nil: no Teams.
	Teams *Teams `yaml:"teams"`
	// Links signs the links of approval cards in chat; Teams' bot needs it.
	Links *ChatLinks `yaml:"links"`
	// Alerts are email addresses that receive every alert (a pull request merged without Quai's
	// approval, enforcement lost), besides the repository's owners: alerts only, never a request's
	// content.
	Alerts []string `yaml:"alerts"`
	// Defaults are the preferences of people who have not chosen theirs on their profile page.
	Defaults NotificationDefaults `yaml:"defaults"`
}

// NotificationDefaults are what people get until they choose otherwise: per channel and kind of
// notification, whether it goes at once, in their daily digest or not at all, and when the digest
// goes.
type NotificationDefaults struct {
	// Channels map a channel (email) to kinds (approval_needed, submitted, approved, rejected,
	// stale, merged, applied, failed, expiring_soon) and their mode (immediate, digest, off). A
	// kind not listed is immediate. Alerts are not a kind one chooses: they always go at once.
	Channels map[string]map[string]string `yaml:"channels"`
	Digest   DigestDefaults               `yaml:"digest"`
}

// DigestDefaults are when the daily digest goes: at Hour (0 to 23, 8 when unset) in TimeZone (an
// IANA name such as Europe/Zurich, UTC when unset).
type DigestDefaults struct {
	Hour     *int   `yaml:"hour"`
	TimeZone string `yaml:"timeZone"`
}

// The vocabulary of notification preferences (docs/design/05-delivery-and-approvals.md#notifications).
var (
	// NotificationChannels are the channels that reach people, which they choose preferences for,
	// configured or not. Teams' channel posts are not among them: they reach a channel, not a
	// person.
	NotificationChannels = []string{"email", "teams"}
	// NotificationKinds are the kinds of notification people choose how they receive, in the
	// order the profile page lists them. Alerts are not among them.
	NotificationKinds = []string{"approval_needed", "submitted", "approved", "rejected", "stale", "merged", "applied", "failed", "expiring_soon"}
	// NotificationModes are how a kind reaches someone on a channel: at once, in their daily
	// digest, or not at all.
	NotificationModes = []string{ModeImmediate, ModeDigest, ModeOff}
)

// Notification modes.
const (
	ModeImmediate = "immediate"
	ModeDigest    = "digest"
	ModeOff       = "off"
)

// DefaultDigestHour is when the digest goes when notifications.defaults.digest.hour is unset.
const DefaultDigestHour = 8

// Channels returns the channels configured that reach people, in NotificationChannels' order:
// email, and Teams when its bot is configured.
func (n *Notifications) Channels() []string {
	var out []string
	if n.Email != nil {
		out = append(out, "email")
	}
	if n.Teams != nil && n.Teams.Bot != nil {
		out = append(out, "teams")
	}
	return out
}

// ChannelEvents are the events a Teams channel may be posted: the kinds of notification, and
// alerts.
var ChannelEvents = append(slices.Clone(NotificationKinds), "alert")

// Teams sends notifications to Microsoft Teams (docs/design/11-integrations.md#notifications):
// channel posts through Workflows webhooks, and personal cards through a bot. Every endpoint is
// https and comes from here, never from what Teams answers.
type Teams struct {
	// Bot sends personal cards, approval cards with Approve and Deny, to people with an Entra ID
	// object ID (oid); nil: no personal cards.
	Bot *TeamsBot `yaml:"bot"`
	// Channels are posted the events chosen, with identifiers only, for the repositories and teams
	// they name.
	Channels []TeamsChannel `yaml:"channels"`
}

// TeamsBot is the Azure Bot Quai sends personal cards as: a single-tenant Entra ID application,
// with a client secret, that the Teams app of the bot is installed for.
type TeamsBot struct {
	AppID    string `yaml:"appId"`    // the application (client) ID
	TenantID string `yaml:"tenantId"` // the directory (tenant) ID
	// ClientSecret is the application's client secret, a secret reference.
	ClientSecret SecretRef `yaml:"clientSecret"`
	// TokenURL is where Quai gets the bot's tokens: Entra ID's v2.0 token endpoint of the tenant
	// by default.
	TokenURL string `yaml:"tokenURL"`
	// Scope is the Bot Connector's: https://api.botframework.com/.default by default.
	Scope string `yaml:"scope"`
	// ServiceURL is the Bot Connector's service for proactive messages: Microsoft's public
	// cloud's https://smba.trafficmanager.net/teams/ by default (another for GCC and DoD).
	ServiceURL string `yaml:"serviceURL"`
}

// The bot's defaults: Microsoft's public cloud.
const (
	DefaultTeamsScope      = "https://api.botframework.com/.default"
	DefaultTeamsServiceURL = "https://smba.trafficmanager.net/teams/"
)

// TeamsChannel is a Teams channel Quai posts to through a Workflows webhook.
type TeamsChannel struct {
	Name string `yaml:"name"` // names it in the notifications table: lowercase letters, digits and dashes
	// Webhook is the URL of the workflow's "When a Teams webhook request is received" trigger,
	// which anyone may call: a credential, so a secret reference.
	Webhook SecretRef `yaml:"webhook"`
	// Repositories and Teams say whose events it gets: requests to these repositories, or to
	// resources these teams own (team:<name> among their owners), and alerts of these
	// repositories. At least one.
	Repositories []string `yaml:"repositories"`
	Teams        []string `yaml:"teams"`
	// Events are the events posted: kinds of notification (submitted, approval_needed, approved,
	// rejected, stale, merged, applied, failed, expiring_soon) and alert.
	Events []string `yaml:"events"`
}

// ChatLinks signs the links of approval cards in chat: the key, a secret of at least 32 bytes
// used for nothing else, and the keys before it, which open the links they signed until those
// expire (a day): rotate by naming the new key key and the old one in previousKeys for a day.
type ChatLinks struct {
	Key          SecretRef   `yaml:"key"`
	PreviousKeys []SecretRef `yaml:"previousKeys"`
}

// timeZoneRe is an IANA time zone's name: one to three parts of letters, digits, _, + and -,
// the first starting with a letter (Europe/Zurich, America/Argentina/Buenos_Aires, Etc/GMT+5,
// UTC). No dots, so no path; Local, Go's name for the machine's zone, is refused apart.
var timeZoneRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]{0,31}(/[A-Za-z0-9][A-Za-z0-9_+-]{0,31}){0,2}$`)

// TimeZone returns the time zone of an IANA name, from the system's database or the one built
// into Quai. It refuses anything else: an empty name, Local, a path.
func TimeZone(name string) (*time.Location, error) {
	if !timeZoneRe.MatchString(name) || name == "Local" {
		return nil, fmt.Errorf("%q is not an IANA time zone name, such as Europe/Zurich", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%q is not a known time zone, such as Europe/Zurich", name)
	}
	return loc, nil
}

// Email is the SMTP server notifications are sent through, over TLS: STARTTLS, which the server
// must offer (never a fall back to plaintext), or TLS from the first byte. The server's
// certificate is verified against the system's CAs and network.caFile.
type Email struct {
	Host string `yaml:"host"`
	// Port is 587 (submission) with starttls and 465 with tls by default.
	Port int `yaml:"port"`
	// Transport is starttls (the default) or tls.
	Transport string `yaml:"transport"`
	// From is the messages' sender: an address, with a display name or not
	// ("Quai <quai@example.com>").
	From string `yaml:"from"`
	// Username and Password authenticate to the server (PLAIN, or LOGIN when it offers only
	// that); none: no authentication. The password is a secret reference.
	Username string    `yaml:"username"`
	Password SecretRef `yaml:"password"`
}

// Email transports.
const (
	EmailSTARTTLS = "starttls"
	EmailTLS      = "tls"
)

// Server configures the HTTP server.
type Server struct {
	Addr string `yaml:"addr"` // listen address, such as :8080
	// PublicURL is where people reach Quai, for the links in pull requests; empty: no links.
	PublicURL       string        `yaml:"publicURL"`
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"` // how long requests in flight may finish
}

// Database configures PostgreSQL.
type Database struct {
	URL string `yaml:"url"` // such as postgres://quai@db:5432/quai; secrets belong in QUAI_DATABASE_URL
	// MigrateURL is how quai migrate connects: as the schema's owner, while URL names the
	// application role the server and the worker connect as, which may only read and append the
	// audit log (open question 19). Empty: URL, one role for both, and the log is not protected.
	MigrateURL string `yaml:"migrateURL"`
}

// Git configures how Quai works with repositories.
type Git struct {
	CacheDir  string    `yaml:"cacheDir"` // where repository mirrors live; the user cache directory by default
	Committer Committer `yaml:"committer"`
	// SigningKey names the SSH private key Quai signs its commits with, in a secret store
	// (k8s:quai/signing-key#ssh-privatekey, env:NAME); none: commits are unsigned.
	SigningKey struct {
		Ref string `yaml:"ref"`
	} `yaml:"signingKey"`
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

// Webhooks configures where the Git platforms deliver webhooks.
type Webhooks struct {
	// Host, when set, is the only host name /webhooks/ answers on (and that host answers only
	// webhooks, run reports and /healthz), to expose it to the platforms alone.
	Host string `yaml:"host"`
}

// Audit configures the audit log (docs/design/09-security-and-compliance.md#audit-log).
type Audit struct {
	// Retention is how long events are kept, 8760h (a year) by default: quai migrate sets it in
	// the database, and the worker prunes older events, those before a signed checkpoint.
	Retention time.Duration `yaml:"retention"`
	// Checkpoints configures the signed checkpoints the worker writes every hour and at shutdown.
	Checkpoints AuditCheckpoints `yaml:"checkpoints"`
	// Export lists the SIEMs the worker exports the audit log to, each event at least once, in
	// order.
	Export []AuditExport `yaml:"export"`
}

// AuditExport is a SIEM the audit log is exported to: syslog, OpenTelemetry logs or Splunk's HTTP
// Event Collector, exactly one of them (docs/design/09-security-and-compliance.md#export-to-a-siem).
type AuditExport struct {
	// Name names the destination, in metrics and logs; the database keeps how far its export got
	// under it, so renaming it exports the log again from the first event kept.
	Name   string        `yaml:"name"`
	Syslog *SyslogExport `yaml:"syslog"`
	OTLP   *OTLPExport   `yaml:"otlp"`
	Splunk *SplunkExport `yaml:"splunk"`
}

// SyslogExport sends RFC 5424 messages over TCP, framed by octet counting (RFC 6587, RFC 5425).
type SyslogExport struct {
	Address string `yaml:"address"` // host:port
	// Transport is tls (RFC 5425, the default), or tcp, which production refuses but to a
	// loopback address.
	Transport string `yaml:"transport"`
	// Facility is the messages' facility, 0 to 23: 13 (log audit) by default.
	Facility *int `yaml:"facility"`
}

// OTLPExport sends OpenTelemetry logs over OTLP/HTTP, in protobuf.
type OTLPExport struct {
	// Endpoint is the collector's base URL: logs go to its /v1/logs unless the URL names another
	// path. https; http only in development or to a loopback address.
	Endpoint string `yaml:"endpoint"`
	// Headers are sent with each request, such as an API key: secret references.
	Headers map[string]SecretRef `yaml:"headers"`
}

// SplunkExport sends events to a Splunk HTTP Event Collector (/services/collector/event).
type SplunkExport struct {
	// URL is the collector's base URL, such as https://splunk.example.com:8088. https; http only in
	// development or to a loopback address.
	URL string `yaml:"url"`
	// Token is the HEC token, a secret reference.
	Token SecretRef `yaml:"token"`
	// Index and Sourcetype are the events'; empty: the token's defaults.
	Index      string `yaml:"index"`
	Sourcetype string `yaml:"sourcetype"`
}

// SecretRef names a secret in a secret store: k8s:namespace/name#key, env:NAME.
type SecretRef struct {
	Ref string `yaml:"ref"`
}

// Syslog transports.
const (
	SyslogTLS = "tls"
	SyslogTCP = "tcp"
)

// DefaultSyslogFacility is log audit (RFC 5424, section 6.2.1).
const DefaultSyslogFacility = 13

// AuditCheckpoints configures the audit log's checkpoints.
type AuditCheckpoints struct {
	// SigningKey names the Ed25519 private key the worker signs checkpoints with (PEM, PKCS #8),
	// in a secret store (k8s:quai/audit-key#private.pem, env:NAME); none: no checkpoints, so
	// nothing is pruned and verification cannot tell a rewritten chain.
	SigningKey struct {
		Ref string `yaml:"ref"`
	} `yaml:"signingKey"`
	// PreviousKeys are the public keys (PEM) of the signing keys before a rotation: checkpoints
	// they signed still verify.
	PreviousKeys []string `yaml:"previousKeys"`
	// Repository names the repository connection each checkpoint is committed to, an external,
	// timestamped anchor, with delivery mode direct; none: checkpoints stay in the database.
	Repository string `yaml:"repository"`
}

// MinAuditRetention is the shortest retention: a day, the period of the chain's verification.
const MinAuditRetention = 24 * time.Hour

// Network configures outgoing connections: Git, platform APIs, the identity provider, traces.
// The proxy comes from the standard environment variables (HTTPS_PROXY, HTTP_PROXY, NO_PROXY).
type Network struct {
	// CAFile is a PEM file of CA certificates trusted in addition to the system's: an internal
	// CA, typically.
	CAFile string `yaml:"caFile"`
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

// Auth configures who can sign in, the roles their groups give them and the teams they are in.
type Auth struct {
	// Mode is oidc, or dev for the development user switcher, which production refuses.
	Mode    string        `yaml:"mode"`
	OIDC    OIDC          `yaml:"oidc"`
	Roles   []RoleMapping `yaml:"roles"`
	Teams   []TeamMapping `yaml:"teams"`
	Session Session       `yaml:"session"`
	Dev     Dev           `yaml:"dev"`
	// StepUp configures step-up authentication; nil: not configured, and only approval rules
	// with stepUp ask for one, with the default maximum age.
	StepUp *StepUp `yaml:"stepUp"`
}

// StepUp configures step-up authentication (docs/design/08-identity-access-tenancy.md#step-up):
// how recently, and how, a person must have authenticated with the identity provider to approve
// a request whose approval rules ask for it, to break glass, and, with Admin, to change
// repository connections.
type StepUp struct {
	// MaxAge is how long an authentication satisfies a step-up: DefaultStepUpMaxAge by default.
	// It is the ID token's auth_time that counts, never when Quai started the session.
	MaxAge time.Duration `yaml:"maxAge"`
	// ACRValues, when set, are sent as acr_values, and the ID token's acr must be one of them.
	ACRValues []string `yaml:"acrValues"`
	// AuthContext, when set, is the Entra ID Conditional Access authentication context (c1 to
	// c99) requested in the claims parameter, which the ID token's acrs claim must contain.
	AuthContext string `yaml:"authContext"`
	// Admin makes changing repository connections need a step-up too.
	Admin bool `yaml:"admin"`
}

// DefaultStepUpMaxAge is how long an authentication satisfies a step-up when auth.stepUp.maxAge
// is not set (open question 49).
const DefaultStepUpMaxAge = 5 * time.Minute

// StepUpSettings returns the step-up settings in force: auth.stepUp with its defaults, or the
// defaults alone when it is not set.
func (a Auth) StepUpSettings() StepUp {
	s := StepUp{}
	if a.StepUp != nil {
		s = *a.StepUp
	}
	if s.MaxAge == 0 {
		s.MaxAge = DefaultStepUpMaxAge
	}
	return s
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

// TeamMapping maps IdP groups to a Quai team (docs/design/08-identity-access-tenancy.md#directory):
// Group is a group's name, or a pattern with one *, which matches one or more characters; a * in
// Team stands for what it matched ({group: sg-quai-*, team: "*"}: sg-quai-shop is team shop). The
// first entry that matches a group decides its team; a group no entry matches is the team of its
// own name.
type TeamMapping struct {
	Group string `yaml:"group"`
	Team  string `yaml:"team"`
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
	Email  string   `yaml:"email"` // matches owners the Git platform names by email
	Groups []string `yaml:"groups"`
	// ObjectID is the user's Entra ID object ID (oid), which Teams' personal cards are sent to.
	ObjectID string `yaml:"objectId"`
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
	{"QUAI_DATABASE_MIGRATE_URL", func(c *Config, v string) error { c.Database.MigrateURL = v; return nil }},
	{"QUAI_AUTH_MODE", func(c *Config, v string) error { c.Auth.Mode = v; return nil }},
	{"QUAI_AUTH_OIDC_ISSUER", func(c *Config, v string) error { c.Auth.OIDC.Issuer = v; return nil }},
	{"QUAI_AUTH_OIDC_CLIENT_ID", func(c *Config, v string) error { c.Auth.OIDC.ClientID = v; return nil }},
	{"QUAI_AUTH_OIDC_CLIENT_SECRET", func(c *Config, v string) error { c.Auth.OIDC.ClientSecret = v; return nil }},
	{"QUAI_AUTH_OIDC_REDIRECT_URL", func(c *Config, v string) error { c.Auth.OIDC.RedirectURL = v; return nil }},
	{"QUAI_GIT_CACHE_DIR", func(c *Config, v string) error { c.Git.CacheDir = v; return nil }},
	{"QUAI_GIT_SIGNING_KEY_REF", func(c *Config, v string) error { c.Git.SigningKey.Ref = v; return nil }},
	{"QUAI_LOG_LEVEL", func(c *Config, v string) error { c.Log.Level = v; return nil }},
	{"QUAI_LOG_FORMAT", func(c *Config, v string) error { c.Log.Format = v; return nil }},
	{"QUAI_TELEMETRY_OTLP_ENDPOINT", func(c *Config, v string) error { c.Telemetry.OTLPEndpoint = v; return nil }},
	{"QUAI_TELEMETRY_SERVICE_NAME", func(c *Config, v string) error { c.Telemetry.ServiceName = v; return nil }},
	{"QUAI_REPOSITORIES_PATH", func(c *Config, v string) error { c.Repositories.Path = v; return nil }},
	{"QUAI_SECRETS_KUBECONFIG", func(c *Config, v string) error { c.Secrets.Kubernetes.Kubeconfig = v; return nil }},
	{"QUAI_NETWORK_CA_FILE", func(c *Config, v string) error { c.Network.CAFile = v; return nil }},
	{"QUAI_SERVER_PUBLIC_URL", func(c *Config, v string) error { c.Server.PublicURL = v; return nil }},
	{"QUAI_WEBHOOKS_HOST", func(c *Config, v string) error { c.Webhooks.Host = v; return nil }},
	{"QUAI_AUDIT_RETENTION", func(c *Config, v string) (err error) {
		c.Audit.Retention, err = time.ParseDuration(v)
		return err
	}},
	{"QUAI_AUDIT_CHECKPOINTS_SIGNING_KEY_REF", func(c *Config, v string) error { c.Audit.Checkpoints.SigningKey.Ref = v; return nil }},
	{"QUAI_AUDIT_CHECKPOINTS_REPOSITORY", func(c *Config, v string) error { c.Audit.Checkpoints.Repository = v; return nil }},
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
	if c.Audit.Retention == 0 {
		c.Audit.Retention = 365 * 24 * time.Hour
	}
	for _, d := range c.Audit.Export {
		if d.Syslog != nil && d.Syslog.Transport == "" {
			d.Syslog.Transport = SyslogTLS
		}
		if d.Syslog != nil && d.Syslog.Facility == nil {
			f := DefaultSyslogFacility
			d.Syslog.Facility = &f
		}
	}
	if d := &c.Notifications.Defaults.Digest; d.Hour == nil {
		h := DefaultDigestHour
		d.Hour = &h
	}
	if d := &c.Notifications.Defaults.Digest; d.TimeZone == "" {
		d.TimeZone = "UTC"
	}
	if e := c.Notifications.Email; e != nil {
		if e.Transport == "" {
			e.Transport = EmailSTARTTLS
		}
		if e.Port == 0 {
			e.Port = 587
			if e.Transport == EmailTLS {
				e.Port = 465
			}
		}
	}
	if t := c.Notifications.Teams; t != nil && t.Bot != nil {
		b := t.Bot
		if b.TokenURL == "" && b.TenantID != "" {
			b.TokenURL = "https://login.microsoftonline.com/" + url.PathEscape(b.TenantID) + "/oauth2/v2.0/token"
		}
		if b.Scope == "" {
			b.Scope = DefaultTeamsScope
		}
		if b.ServiceURL == "" {
			b.ServiceURL = DefaultTeamsServiceURL
		}
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
	if ref := c.Git.SigningKey.Ref; ref != "" && !signingRefRe.MatchString(ref) {
		errs = append(errs, fmt.Errorf("git.signingKey.ref must be a secret reference such as k8s:namespace/name#key or env:NAME, not %q", ref))
	}
	errs = append(errs, c.Audit.validate(c.Env))
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
	if u := c.Database.MigrateURL; u != "" {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "postgres" && p.Scheme != "postgresql") {
			errs = append(errs, errors.New("database.migrateURL must be a postgres:// URL"))
		}
	}
	if c.Server.ShutdownTimeout < 0 {
		errs = append(errs, errors.New("server.shutdownTimeout must not be negative"))
	}
	errs = append(errs, c.Auth.validate(c.Env))
	errs = append(errs, c.Notifications.validate(c.Server.PublicURL))
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

func (a *Audit) validate(env string) error {
	var errs []error
	names := map[string]bool{}
	for i, d := range a.Export {
		if !exportNameRe.MatchString(d.Name) {
			errs = append(errs, fmt.Errorf("audit.export[%d].name must be lowercase letters, digits and dashes, not %q", i, d.Name))
		} else if names[d.Name] {
			errs = append(errs, fmt.Errorf("audit.export[%d].name: %s names another destination already", i, d.Name))
		}
		names[d.Name] = true
		errs = append(errs, d.validate(fmt.Sprintf("audit.export[%d]", i), env))
	}
	if a.Retention < MinAuditRetention {
		errs = append(errs, fmt.Errorf("audit.retention must be at least %s, not %s", MinAuditRetention, a.Retention))
	}
	cp := a.Checkpoints
	if ref := cp.SigningKey.Ref; ref != "" && !signingRefRe.MatchString(ref) {
		errs = append(errs, fmt.Errorf("audit.checkpoints.signingKey.ref must be a secret reference such as k8s:namespace/name#key or env:NAME, not %q", ref))
	}
	if cp.Repository != "" && cp.SigningKey.Ref == "" {
		errs = append(errs, errors.New("audit.checkpoints.repository needs audit.checkpoints.signingKey.ref: there is no checkpoint to commit without it"))
	}
	for i, k := range cp.PreviousKeys {
		if err := ed25519PublicKey(k); err != nil {
			errs = append(errs, fmt.Errorf("audit.checkpoints.previousKeys[%d]: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

var exportNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// headerNameRe is an HTTP header name, an RFC 9110 token.
var headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func (d AuditExport) validate(key, env string) error {
	n := 0
	for _, set := range []bool{d.Syslog != nil, d.OTLP != nil, d.Splunk != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("%s must have exactly one of syslog, otlp and splunk", key)
	}
	var errs []error
	switch {
	case d.Syslog != nil:
		s := d.Syslog
		host, port, err := net.SplitHostPort(s.Address)
		if err != nil || host == "" || port == "" {
			errs = append(errs, fmt.Errorf("%s.syslog.address must be host:port, not %q", key, s.Address))
		}
		switch s.Transport {
		case SyslogTLS:
		case SyslogTCP:
			if env == Production && !loopback(host) {
				errs = append(errs, fmt.Errorf("%s.syslog.transport tcp sends the audit log unencrypted: production refuses it but to a loopback address", key))
			}
		default:
			errs = append(errs, fmt.Errorf("%s.syslog.transport must be tls or tcp, not %q", key, s.Transport))
		}
		if f := s.Facility; f != nil && (*f < 0 || *f > 23) {
			errs = append(errs, fmt.Errorf("%s.syslog.facility must be 0 to 23, not %d", key, *f))
		}
	case d.OTLP != nil:
		errs = append(errs, exportURL(key+".otlp.endpoint", d.OTLP.Endpoint, env))
		for name, h := range d.OTLP.Headers {
			if !headerNameRe.MatchString(name) {
				errs = append(errs, fmt.Errorf("%s.otlp.headers: %q is not a header name", key, name))
			}
			if !signingRefRe.MatchString(h.Ref) {
				errs = append(errs, fmt.Errorf("%s.otlp.headers.%s.ref must be a secret reference such as k8s:namespace/name#key or env:NAME", key, name))
			}
		}
	case d.Splunk != nil:
		errs = append(errs, exportURL(key+".splunk.url", d.Splunk.URL, env))
		if !signingRefRe.MatchString(d.Splunk.Token.Ref) {
			errs = append(errs, fmt.Errorf("%s.splunk.token.ref must be a secret reference such as k8s:namespace/name#key or env:NAME", key))
		}
	}
	return errors.Join(errs...)
}

// exportURL checks a destination's URL: https, or http in development or to a loopback address.
func exportURL(key, s, env string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return fmt.Errorf("%s must be an https URL without credentials, not %q", key, s)
	}
	if u.Scheme == "http" && env == Production && !loopback(u.Hostname()) {
		return fmt.Errorf("%s: http sends the audit log unencrypted: production refuses it but to a loopback address", key)
	}
	return nil
}

func (n *Notifications) validate(publicURL string) error {
	var errs []error
	if e := n.Email; e != nil {
		if e.Host == "" || strings.ContainsAny(e.Host, " \t\r\n/:") && net.ParseIP(e.Host) == nil {
			errs = append(errs, fmt.Errorf("notifications.email.host must be a host name or an address, not %q", e.Host))
		}
		if e.Port < 1 || e.Port > 65535 {
			errs = append(errs, fmt.Errorf("notifications.email.port must be 1 to 65535, not %d", e.Port))
		}
		if e.Transport != EmailSTARTTLS && e.Transport != EmailTLS {
			// No plaintext: notifications carry reasons, diffs and plans (open question 52).
			errs = append(errs, fmt.Errorf("notifications.email.transport must be starttls or tls, not %q", e.Transport))
		}
		if _, err := MailAddress(e.From, true); err != nil {
			errs = append(errs, fmt.Errorf("notifications.email.from: %w", err))
		}
		switch {
		case e.Username == "" && e.Password.Ref != "":
			errs = append(errs, errors.New("notifications.email.password needs notifications.email.username"))
		case e.Username != "" && !signingRefRe.MatchString(e.Password.Ref):
			errs = append(errs, errors.New("notifications.email.password.ref must be a secret reference such as k8s:namespace/name#key or env:NAME"))
		case strings.ContainsAny(e.Username, "\r\n\x00"):
			errs = append(errs, errors.New("notifications.email.username must be one line"))
		}
		// Every message links to its request: without the address people reach Quai at, there is
		// no link to give (open question 52).
		if u, err := url.Parse(publicURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			errs = append(errs, errors.New("notifications.email needs server.publicURL, an http or https URL: the messages link to Quai"))
		}
	}
	for i, a := range n.Alerts {
		if _, err := MailAddress(a, false); err != nil {
			errs = append(errs, fmt.Errorf("notifications.alerts[%d]: %w", i, err))
		}
	}
	if len(n.Alerts) > 0 && n.Email == nil {
		errs = append(errs, errors.New("notifications.alerts are email addresses: they need notifications.email"))
	}
	if n.Teams != nil {
		errs = append(errs, n.Teams.validate(publicURL, n.Links != nil))
	}
	if l := n.Links; l != nil {
		if !signingRefRe.MatchString(l.Key.Ref) {
			errs = append(errs, errors.New("notifications.links.key.ref must be a secret reference such as k8s:namespace/name#key or env:NAME"))
		}
		for i, k := range l.PreviousKeys {
			if !signingRefRe.MatchString(k.Ref) {
				errs = append(errs, fmt.Errorf("notifications.links.previousKeys[%d].ref must be a secret reference such as k8s:namespace/name#key or env:NAME", i))
			}
		}
	}
	errs = append(errs, n.Defaults.validate(n.Channels()))
	return errors.Join(errs...)
}

func (t *Teams) validate(publicURL string, links bool) error {
	var errs []error
	if t.Bot == nil && len(t.Channels) == 0 {
		errs = append(errs, errors.New("notifications.teams needs a bot, channels, or both"))
	}
	// Cards link to Quai: without the address people reach it at, there is nothing to link.
	if u, err := url.Parse(publicURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		errs = append(errs, errors.New("notifications.teams needs server.publicURL, an http or https URL: the cards link to Quai"))
	}
	if b := t.Bot; b != nil {
		if _, err := uuid.Parse(b.AppID); err != nil {
			errs = append(errs, fmt.Errorf("notifications.teams.bot.appId must be the application (client) ID, a GUID, not %q", b.AppID))
		}
		if _, err := uuid.Parse(b.TenantID); err != nil {
			errs = append(errs, fmt.Errorf("notifications.teams.bot.tenantId must be the directory (tenant) ID, a GUID, not %q", b.TenantID))
		}
		if !signingRefRe.MatchString(b.ClientSecret.Ref) {
			errs = append(errs, errors.New("notifications.teams.bot.clientSecret.ref must be a secret reference such as k8s:namespace/name#key or env:NAME"))
		}
		errs = append(errs, httpsURL("notifications.teams.bot.tokenURL", b.TokenURL), httpsURL("notifications.teams.bot.serviceURL", b.ServiceURL))
		if u, err := url.Parse(b.Scope); err != nil || u.Scheme != "https" || u.Host == "" || strings.ContainsAny(b.Scope, " \t\r\n") {
			errs = append(errs, fmt.Errorf("notifications.teams.bot.scope must be one https scope, such as %s, not %q", DefaultTeamsScope, b.Scope))
		}
		if !links {
			// Approval cards carry signed links (open question 61).
			errs = append(errs, errors.New("notifications.teams.bot needs notifications.links.key: approval cards carry signed links"))
		}
	}
	names := map[string]bool{}
	for i, c := range t.Channels {
		key := fmt.Sprintf("notifications.teams.channels[%d]", i)
		if !exportNameRe.MatchString(c.Name) {
			errs = append(errs, fmt.Errorf("%s.name must be lowercase letters, digits and dashes, not %q", key, c.Name))
		} else if names[c.Name] {
			errs = append(errs, fmt.Errorf("%s.name: %s names another channel already", key, c.Name))
		}
		names[c.Name] = true
		if !signingRefRe.MatchString(c.Webhook.Ref) {
			errs = append(errs, fmt.Errorf("%s.webhook.ref must be a secret reference such as k8s:namespace/name#key or env:NAME: the URL is a credential", key))
		}
		if len(c.Repositories) == 0 && len(c.Teams) == 0 {
			errs = append(errs, fmt.Errorf("%s needs repositories, teams, or both: they say whose events it gets", key))
		}
		for j, r := range c.Repositories {
			if r == "" {
				errs = append(errs, fmt.Errorf("%s.repositories[%d] is empty", key, j))
			}
		}
		for j, team := range c.Teams {
			if team == "" {
				errs = append(errs, fmt.Errorf("%s.teams[%d] is empty", key, j))
			}
		}
		if len(c.Events) == 0 {
			errs = append(errs, fmt.Errorf("%s.events: name at least one (%s)", key, strings.Join(ChannelEvents, ", ")))
		}
		for _, e := range c.Events {
			if !slices.Contains(ChannelEvents, e) {
				errs = append(errs, fmt.Errorf("%s.events: %q is not an event (%s)", key, e, strings.Join(ChannelEvents, ", ")))
			}
		}
	}
	return errors.Join(errs...)
}

// httpsURL checks an endpoint of a service Quai calls with credentials: https, without
// credentials, a query or a fragment.
func httpsURL(key, s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must be an https URL without credentials or a query, not %q", key, s)
	}
	return nil
}

func (d *NotificationDefaults) validate(configured []string) error {
	var errs []error
	for _, channel := range slices.Sorted(maps.Keys(d.Channels)) {
		key := "notifications.defaults.channels." + channel
		switch {
		case !slices.Contains(NotificationChannels, channel):
			errs = append(errs, fmt.Errorf("notifications.defaults.channels: %q is not a channel (%s)", channel, strings.Join(NotificationChannels, ", ")))
			continue
		case !slices.Contains(configured, channel):
			errs = append(errs, fmt.Errorf("%s needs notifications.%s: the channel is not configured", key, channel))
		}
		for _, kind := range slices.Sorted(maps.Keys(d.Channels[channel])) {
			mode := d.Channels[channel][kind]
			switch {
			case kind == "alert":
				errs = append(errs, fmt.Errorf("%s.alert: alerts always go at once; they are not a kind one chooses", key))
			case !slices.Contains(NotificationKinds, kind):
				errs = append(errs, fmt.Errorf("%s: %q is not a kind of notification (%s)", key, kind, strings.Join(NotificationKinds, ", ")))
			case !slices.Contains(NotificationModes, mode):
				errs = append(errs, fmt.Errorf("%s.%s must be immediate, digest or off, not %q", key, kind, mode))
			}
		}
	}
	// Approvers hear of the requests that wait for them on some channel: approval_needed cannot be
	// off on every one.
	if len(configured) > 0 && !slices.ContainsFunc(configured, func(c string) bool { return d.Channels[c]["approval_needed"] != ModeOff }) {
		errs = append(errs, errors.New("notifications.defaults.channels: approval_needed cannot be off on every channel: approvers must hear of the requests that wait for them"))
	}
	if h := d.Digest.Hour; h != nil && (*h < 0 || *h > 23) {
		errs = append(errs, fmt.Errorf("notifications.defaults.digest.hour must be 0 to 23, not %d", *h))
	}
	if _, err := TimeZone(d.Digest.TimeZone); err != nil {
		errs = append(errs, fmt.Errorf("notifications.defaults.digest.timeZone: %w", err))
	}
	return errors.Join(errs...)
}

// MailAddress parses an email address, with a display name when named is true
// ("Quai <quai@example.com>"), and only the address otherwise. It refuses what could add a line
// to a message's header.
func MailAddress(s string, named bool) (*mail.Address, error) {
	if strings.ContainsAny(s, "\r\n\x00") {
		return nil, fmt.Errorf("%q must be one line", s)
	}
	a, err := mail.ParseAddress(s)
	if err != nil {
		return nil, fmt.Errorf("%q is not an email address", s)
	}
	if !named && (a.Name != "" || a.Address != strings.TrimSpace(s)) {
		return nil, fmt.Errorf("%q must be an address alone, such as secops@example.com", s)
	}
	return a, nil
}

// loopback reports whether host is this machine: localhost or a loopback address.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ed25519PublicKey checks a PEM Ed25519 public key, as `openssl pkey -pubout` writes it.
func ed25519PublicKey(s string) error {
	block, rest := pem.Decode([]byte(s))
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) > 0 {
		return errors.New("must be one PEM \"PUBLIC KEY\" (openssl pkey -pubout)")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return err
	}
	if _, ok := k.(ed25519.PublicKey); !ok {
		return errors.New("is not an Ed25519 key")
	}
	return nil
}

// roles are the role names of docs/design/08; internal/authz defines them.
var signingRefRe = regexp.MustCompile(`^(env|k8s|vault|azkv):\S+$`)

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
	for i, m := range a.Teams {
		stars := strings.Count(m.Group, "*")
		switch {
		case m.Group == "" || m.Team == "":
			errs = append(errs, fmt.Errorf("auth.teams[%d] needs a group and a team", i))
		case stars > 1:
			errs = append(errs, fmt.Errorf("auth.teams[%d].group %q may hold one * at most", i, m.Group))
		case strings.Count(m.Team, "*") > stars:
			errs = append(errs, fmt.Errorf("auth.teams[%d].team %q may hold a * only once, and only when its group has one", i, m.Team))
		}
	}
	if a.Session.Lifetime < 0 || a.Session.IdleTimeout < 0 {
		errs = append(errs, errors.New("auth.session durations must not be negative"))
	}
	if s := a.StepUp; s != nil {
		errs = append(errs, s.validate(a.Mode))
	}
	return errors.Join(errs...)
}

var authContextRe = regexp.MustCompile(`^c([1-9]|[1-9][0-9])$`)

func (s *StepUp) validate(mode string) error {
	var errs []error
	if s.MaxAge < 0 || (s.MaxAge > 0 && s.MaxAge < time.Second) {
		errs = append(errs, fmt.Errorf("auth.stepUp.maxAge must be at least 1s, not %s", s.MaxAge))
	}
	for i, v := range s.ACRValues {
		if v == "" || strings.ContainsAny(v, " \t\r\n") {
			errs = append(errs, fmt.Errorf("auth.stepUp.acrValues[%d] must be a non-empty value without spaces, not %q", i, v))
		}
	}
	if s.AuthContext != "" && !authContextRe.MatchString(s.AuthContext) {
		errs = append(errs, fmt.Errorf("auth.stepUp.authContext must be an Entra ID authentication context, c1 to c99, not %q", s.AuthContext))
	}
	if mode == AuthDev && (len(s.ACRValues) > 0 || s.AuthContext != "") {
		// The switcher authenticates nobody: no acr to check, so no step-up could ever succeed.
		errs = append(errs, errors.New("auth.stepUp.acrValues and auth.stepUp.authContext need auth.mode oidc: the development switcher has no identity provider to ask"))
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
