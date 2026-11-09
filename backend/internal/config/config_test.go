package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "quai.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaults(t *testing.T) {
	c, err := Load("", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != Development || c.Server.Addr != ":8080" || c.Server.ShutdownTimeout != 20*time.Second ||
		c.Log.Level != "info" || c.Log.Format != "text" || c.Telemetry.ServiceName != "quai" || c.Telemetry.OTLPEndpoint != "" {
		t.Errorf("defaults = %+v", c)
	}
	prod, err := Load("", env(map[string]string{"QUAI_ENV": "production", "QUAI_AUTH_OIDC_ISSUER": "https://idp.example.com", "QUAI_AUTH_OIDC_CLIENT_ID": "quai", "QUAI_AUTH_OIDC_REDIRECT_URL": "https://quai.example.com/auth/callback"}))
	if err != nil || prod.Log.Format != "json" || prod.Auth.Mode != AuthOIDC || prod.Auth.OIDC.GroupsClaim != "groups" || len(prod.Auth.Dev.Users) != 0 {
		t.Errorf("production defaults = %+v, %v", prod, err)
	}
}

func TestFileAndEnvironment(t *testing.T) {
	p := write(t, "env: production\nauth: { oidc: { issuer: \"https://idp\", clientID: quai, redirectURL: \"https://quai/auth/callback\" } }\nserver: { addr: \"127.0.0.1:9000\", shutdownTimeout: 5s }\nlog: { level: debug, format: text }\ntelemetry: { otlpEndpoint: \"http://collector:4318\" }\n")
	c, err := Load(p, env(map[string]string{"QUAI_DATABASE_URL": "postgres://quai@db/quai", "QUAI_DATABASE_MIGRATE_URL": "postgres://owner@db/quai", "QUAI_LOG_LEVEL": "warn", "QUAI_SERVER_SHUTDOWN_TIMEOUT": "1m", "QUAI_TELEMETRY_SERVICE_NAME": "quai-test"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != Production || c.Server.Addr != "127.0.0.1:9000" || c.Server.ShutdownTimeout != time.Minute ||
		c.Database.URL != "postgres://quai@db/quai" || c.Database.MigrateURL != "postgres://owner@db/quai" || c.Log.Level != "warn" || c.Log.Format != "text" || c.Telemetry.OTLPEndpoint != "http://collector:4318" || c.Telemetry.ServiceName != "quai-test" {
		t.Errorf("config = %+v", c)
	}
	// QUAI_CONFIG names the file when --config is not given.
	c, err = Load("", env(map[string]string{"QUAI_CONFIG": p, "QUAI_SERVER_ADDR": ":7000"}))
	if err != nil || c.Server.Addr != ":7000" || c.Env != Production {
		t.Errorf("QUAI_CONFIG = %+v, %v", c, err)
	}
	if c, err := Load(write(t, "# only a comment\n"), env(nil)); err != nil || c.Env != Development {
		t.Errorf("empty file = %+v, %v", c, err)
	}
}

func TestInvalid(t *testing.T) {
	for name, tt := range map[string]struct {
		file string
		env  map[string]string
		want string
	}{
		"unknown key":      {file: "serve: {}\n", want: "field serve not found"},
		"bad YAML":         {file: "env: [\n", want: "configuration "},
		"env":              {env: map[string]string{"QUAI_ENV": "staging"}, want: `env must be development or production, not "staging"`},
		"addr":             {env: map[string]string{"QUAI_SERVER_ADDR": "8080"}, want: "server.addr must be host:port"},
		"duration":         {env: map[string]string{"QUAI_SERVER_SHUTDOWN_TIMEOUT": "soon"}, want: "QUAI_SERVER_SHUTDOWN_TIMEOUT"},
		"negative timeout": {file: "server: { shutdownTimeout: -1s }\n", want: "must not be negative"},
		"database":         {env: map[string]string{"QUAI_DATABASE_URL": "mysql://x"}, want: "database.url must be a postgres:// URL"},
		"migrate database": {env: map[string]string{"QUAI_DATABASE_MIGRATE_URL": "mysql://x"}, want: "database.migrateURL must be a postgres:// URL"},
		"level":            {env: map[string]string{"QUAI_LOG_LEVEL": "loud"}, want: `log.level must be debug, info, warn or error, not "loud"`},
		"format":           {env: map[string]string{"QUAI_LOG_FORMAT": "xml"}, want: `log.format must be json or text`},
		"endpoint":         {env: map[string]string{"QUAI_TELEMETRY_OTLP_ENDPOINT": "collector:4318"}, want: "telemetry.otlpEndpoint must be an http or https URL"},
		"retention":        {env: map[string]string{"QUAI_AUDIT_RETENTION": "1h"}, want: "audit.retention must be at least 24h0m0s"},
		"retention format": {file: "audit: { retention: 1y }\n", want: "configuration "},
		"checkpoint key":   {env: map[string]string{"QUAI_AUDIT_CHECKPOINTS_SIGNING_KEY_REF": "/etc/key.pem"}, want: "audit.checkpoints.signingKey.ref must be a secret reference"},
		"audit repository": {env: map[string]string{"QUAI_AUDIT_CHECKPOINTS_REPOSITORY": "audit"}, want: "audit.checkpoints.repository needs audit.checkpoints.signingKey.ref"},
		"previous key":     {file: "audit: { checkpoints: { previousKeys: [\"not a key\"] } }\n", want: "audit.checkpoints.previousKeys[0]: must be one PEM"},
		"previous RSA key": {file: "audit: { checkpoints: { previousKeys: [\"" + strings.ReplaceAll(rsaPublic, "\n", "\\n") + "\"] } }\n", want: "audit.checkpoints.previousKeys[0]: is not an Ed25519 key"},
		"export name":      {file: "audit: { export: [{ name: SIEM, syslog: { address: siem:6514 } }] }\n", want: `audit.export[0].name must be lowercase letters, digits and dashes, not "SIEM"`},
		"export twice":     {file: "audit: { export: [{ name: siem, syslog: { address: siem:6514 } }, { name: siem, syslog: { address: other:6514 } }] }\n", want: "audit.export[1].name: siem names another destination already"},
		"export none":      {file: "audit: { export: [{ name: siem }] }\n", want: "audit.export[0] must have exactly one of syslog, otlp and splunk"},
		"export two":       {file: "audit: { export: [{ name: siem, syslog: { address: siem:6514 }, otlp: { endpoint: https://otel } }] }\n", want: "audit.export[0] must have exactly one of syslog, otlp and splunk"},
		"syslog address":   {file: "audit: { export: [{ name: siem, syslog: { address: siem } }] }\n", want: `audit.export[0].syslog.address must be host:port, not "siem"`},
		"syslog transport": {file: "audit: { export: [{ name: siem, syslog: { address: siem:514, transport: udp } }] }\n", want: `audit.export[0].syslog.transport must be tls or tcp, not "udp"`},
		"syslog facility":  {file: "audit: { export: [{ name: siem, syslog: { address: siem:6514, facility: 24 } }] }\n", want: "audit.export[0].syslog.facility must be 0 to 23, not 24"},
		"syslog tcp":       {file: "audit: { export: [{ name: siem, syslog: { address: siem:514, transport: tcp } }] }\n", env: map[string]string{"QUAI_ENV": "production"}, want: "audit.export[0].syslog.transport tcp sends the audit log unencrypted"},
		"otlp endpoint":    {file: "audit: { export: [{ name: otel, otlp: { endpoint: otel:4318 } }] }\n", want: "audit.export[0].otlp.endpoint must be an https URL"},
		"otlp http":        {file: "audit: { export: [{ name: otel, otlp: { endpoint: http://otel:4318 } }] }\n", env: map[string]string{"QUAI_ENV": "production"}, want: "audit.export[0].otlp.endpoint: http sends the audit log unencrypted"},
		"otlp header":      {file: "audit: { export: [{ name: otel, otlp: { endpoint: https://otel, headers: { Authorization: { ref: Bearer x } } } }] }\n", want: "audit.export[0].otlp.headers.Authorization.ref must be a secret reference"},
		"otlp header name": {file: "audit: { export: [{ name: otel, otlp: { endpoint: https://otel, headers: { \"a b\": { ref: env:X } } } }] }\n", want: `audit.export[0].otlp.headers: "a b" is not a header name`},
		"splunk url":       {file: "audit: { export: [{ name: splunk, splunk: { url: \"https://u:p@splunk:8088\", token: { ref: env:HEC } } }] }\n", want: "audit.export[0].splunk.url must be an https URL without credentials"},
		"splunk token":     {file: "audit: { export: [{ name: splunk, splunk: { url: https://splunk:8088 } }] }\n", want: "audit.export[0].splunk.token.ref must be a secret reference"},
		"splunk http":      {file: "audit: { export: [{ name: splunk, splunk: { url: http://splunk:8088, token: { ref: env:HEC } } }] }\n", env: map[string]string{"QUAI_ENV": "production"}, want: "audit.export[0].splunk.url: http sends the audit log unencrypted"},
	} {
		p := ""
		if tt.file != "" {
			p = write(t, tt.file)
		}
		if _, err := Load(p, env(tt.env)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tt.want)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), env(nil)); err == nil {
		t.Error("a missing file was accepted")
	}
}

// rsaPublic is a public key of another algorithm than Ed25519.
const rsaPublic = `-----BEGIN PUBLIC KEY-----
MFwwDQYJKoZIhvcNAQEBBQADSwAwSAJBAL3n9C2gS1BqJ2OAuWGfmfQ6t0TlCqRm
o7m1z0yOg3xK3Wb1aiwxV0sLhJ2wB0Ilm7v3hSvwj5Fnt2EeE0lJm0kCAwEAAQ==
-----END PUBLIC KEY-----
`

// TestAuditExport: the SIEMs the audit log is exported to, with their defaults; unencrypted
// transports only to a loopback address in production.
func TestAuditExport(t *testing.T) {
	p := write(t, `audit:
  export:
    - name: siem
      syslog: { address: "siem.example.com:6514" }
    - name: local-syslog
      syslog: { address: "127.0.0.1:514", transport: tcp, facility: 10 }
    - name: otel
      otlp:
        endpoint: http://localhost:4318
        headers: { Authorization: { ref: "env:OTLP_AUTH" } }
    - name: splunk
      splunk: { url: "https://splunk.example.com:8088", token: { ref: "k8s:quai/splunk-hec#token" }, index: audit }
`)
	c, err := Load(p, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	ex := c.Audit.Export
	if len(ex) != 4 || ex[0].Syslog.Transport != SyslogTLS || *ex[0].Syslog.Facility != DefaultSyslogFacility ||
		ex[1].Syslog.Transport != SyslogTCP || *ex[1].Syslog.Facility != 10 ||
		ex[2].OTLP.Headers["Authorization"].Ref != "env:OTLP_AUTH" || ex[3].Splunk.Token.Ref != "k8s:quai/splunk-hec#token" || ex[3].Splunk.Index != "audit" {
		t.Errorf("export = %+v", ex)
	}
	prod := map[string]string{"QUAI_ENV": "production", "QUAI_AUTH_OIDC_ISSUER": "https://idp.example.com",
		"QUAI_AUTH_OIDC_CLIENT_ID": "quai", "QUAI_AUTH_OIDC_REDIRECT_URL": "https://quai.example.com/auth/callback"}
	if _, err := Load(p, env(prod)); err != nil {
		t.Errorf("production with unencrypted destinations on loopback addresses: %v", err)
	}
}

// TestAudit: the audit log's retention and checkpoints.
func TestAudit(t *testing.T) {
	c, err := Load("", env(nil))
	if err != nil || c.Audit.Retention != 8760*time.Hour || c.Audit.Checkpoints.SigningKey.Ref != "" {
		t.Errorf("default audit = %+v, %v", c.Audit, err)
	}
	pub := "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAGb9ECWmEzf6FQbrBZ9w7lshQhqowtrbLDFw4rXAxZuE=\n-----END PUBLIC KEY-----\n"
	p := write(t, "audit:\n  retention: 87600h\n  checkpoints:\n    signingKey: { ref: \"k8s:quai/audit-key#private.pem\" }\n    previousKeys:\n      - |\n"+
		"        "+strings.ReplaceAll(strings.TrimSuffix(pub, "\n"), "\n", "\n        ")+"\n    repository: audit\n")
	c, err = Load(p, env(map[string]string{"QUAI_AUDIT_CHECKPOINTS_REPOSITORY": "audit-log"}))
	if err != nil || c.Audit.Retention != 87600*time.Hour || c.Audit.Checkpoints.SigningKey.Ref != "k8s:quai/audit-key#private.pem" ||
		len(c.Audit.Checkpoints.PreviousKeys) != 1 || c.Audit.Checkpoints.PreviousKeys[0] != pub || c.Audit.Checkpoints.Repository != "audit-log" {
		t.Errorf("audit = %+v, %v", c.Audit, err)
	}
	c, err = Load("", env(map[string]string{"QUAI_AUDIT_RETENTION": "720h", "QUAI_AUDIT_CHECKPOINTS_SIGNING_KEY_REF": "env:QUAI_AUDIT_KEY"}))
	if err != nil || c.Audit.Retention != 720*time.Hour || c.Audit.Checkpoints.SigningKey.Ref != "env:QUAI_AUDIT_KEY" {
		t.Errorf("audit from the environment = %+v, %v", c.Audit, err)
	}
}

func TestLogger(t *testing.T) {
	var buf bytes.Buffer
	c := &Config{Log: Log{Level: "warn", Format: "json"}}
	log := c.Logger(&buf)
	log.Info("hidden")
	log.Warn("shown", slog.Int("n", 1))
	if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, `"msg":"shown","n":1`) {
		t.Errorf("json log = %q", out)
	}
	buf.Reset()
	c.Log = Log{Level: "debug", Format: "text"}
	c.Logger(&buf).Debug("text line")
	if !strings.Contains(buf.String(), "level=DEBUG msg=\"text line\"") {
		t.Errorf("text log = %q", buf.String())
	}
}

func TestAuth(t *testing.T) {
	dev, err := Load("", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	a := dev.Auth
	if a.Mode != AuthDev || len(a.Dev.Users) != 4 || len(a.Roles) == 0 || a.Session.Lifetime != 8*time.Hour || a.Session.IdleTimeout != 30*time.Minute {
		t.Errorf("development auth = %+v", a)
	}
	p := write(t, `auth:
  mode: oidc
  oidc: { issuer: "http://localhost:8081", clientID: quai, redirectURL: "http://localhost:8080/auth/callback", groupsClaim: roles }
  roles:
    - { group: sg-quai-admins, role: platform-admin }
    - { group: sg-shop, role: operator, scope: "team:shop" }
`)
	c, err := Load(p, env(map[string]string{"QUAI_AUTH_OIDC_CLIENT_SECRET": "s3cret"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.OIDC.ClientSecret != "s3cret" || c.Auth.OIDC.GroupsClaim != "roles" || len(c.Auth.Roles) != 2 || len(c.Auth.Dev.Users) != 0 {
		t.Errorf("oidc auth = %+v", c.Auth)
	}
	for name, tt := range map[string]struct {
		file string
		env  map[string]string
		want string
	}{
		"dev in production":         {env: map[string]string{"QUAI_ENV": "production", "QUAI_AUTH_MODE": "dev"}, want: "refused in production"},
		"unknown mode":              {env: map[string]string{"QUAI_AUTH_MODE": "ldap"}, want: `auth.mode must be oidc or dev, not "ldap"`},
		"http issuer in production": {env: map[string]string{"QUAI_ENV": "production", "QUAI_AUTH_OIDC_ISSUER": "http://idp", "QUAI_AUTH_OIDC_CLIENT_ID": "q", "QUAI_AUTH_OIDC_REDIRECT_URL": "https://q/cb"}, want: "auth.oidc.issuer must be an https URL"},
		"no client":                 {env: map[string]string{"QUAI_AUTH_MODE": "oidc", "QUAI_AUTH_OIDC_ISSUER": "https://idp", "QUAI_AUTH_OIDC_REDIRECT_URL": "https://q/cb"}, want: "clientID is required"},
		"bad role":                  {file: "auth: { roles: [{ group: g, role: god }] }\n", want: `auth.roles[0].role must be one of`},
		"no group":                  {file: "auth: { roles: [{ role: viewer }] }\n", want: "auth.roles[0].group is required"},
		"bad scope":                 {file: "auth: { roles: [{ group: g, role: viewer, scope: planet }] }\n", want: "auth.roles[0].scope must be global"},
		"nameless dev user":         {file: "auth: { dev: { users: [{ groups: [a] }] } }\n", want: "every user needs a name"},
		"negative session":          {file: "auth: { session: { idleTimeout: -1m } }\n", want: "must not be negative"},
	} {
		p := ""
		if tt.file != "" {
			p = write(t, tt.file)
		}
		if _, err := Load(p, env(tt.env)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tt.want)
		}
	}
}

func TestRepositories(t *testing.T) {
	p := write(t, "repositories: { path: /etc/quai/repositories.d }\ngit: { cacheDir: /var/cache/quai }\n")
	c, err := Load(p, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Repositories.Path != "/etc/quai/repositories.d" || c.Git.CacheDir != "/var/cache/quai" || c.Git.Committer.Name != "Quai" {
		t.Errorf("repositories = %+v, git = %+v", c.Repositories, c.Git)
	}
	if c, err := Load(p, env(map[string]string{"QUAI_NETWORK_CA_FILE": "/etc/quai/ca/ca.crt"})); err != nil || c.Network.CAFile != "/etc/quai/ca/ca.crt" {
		t.Errorf("QUAI_NETWORK_CA_FILE = %+v, %v", c.Network, err)
	}
	if c, err := Load(p, env(map[string]string{"QUAI_REPOSITORIES_PATH": "/run/quai/repositories.yaml"})); err != nil || c.Repositories.Path != "/run/quai/repositories.yaml" {
		t.Errorf("QUAI_REPOSITORIES_PATH = %+v, %v", c.Repositories, err)
	}
	// The list of earlier versions says where its entries went.
	if _, err := Load(write(t, "repositories: [{ name: a, url: x }]\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "Repository documents") {
		t.Errorf("a list of repositories: %v", err)
	}
	if c, err := Load("", env(map[string]string{"QUAI_GIT_CACHE_DIR": "/tmp/m"})); err != nil || c.Git.CacheDir != "/tmp/m" {
		t.Errorf("QUAI_GIT_CACHE_DIR = %+v, %v", c.Git, err)
	}
}
