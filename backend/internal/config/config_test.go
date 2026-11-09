package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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

// TestNotifications: email through an SMTP server over TLS or STARTTLS, never plaintext; the
// password a secret reference; the addresses one line each; a link to Quai in every message.
func TestNotifications(t *testing.T) {
	p := write(t, `server: { publicURL: https://quai.example.com }
notifications:
  email: { host: smtp.example.com, from: "Quai <quai@example.com>", username: quai, password: { ref: "k8s:quai/smtp#password" } }
  alerts: [secops@example.com]
`)
	c, err := Load(p, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if e := c.Notifications.Email; e.Transport != EmailSTARTTLS || e.Port != 587 || e.Password.Ref != "k8s:quai/smtp#password" || len(c.Notifications.Alerts) != 1 {
		t.Errorf("notifications = %+v, %+v", c.Notifications, e)
	}
	c, err = Load(write(t, "server: { publicURL: https://quai.example.com }\nnotifications: { email: { host: smtp.example.com, transport: tls, from: quai@example.com } }\n"), env(nil))
	if err != nil || c.Notifications.Email.Port != 465 {
		t.Errorf("implicit TLS = %+v, %v", c.Notifications.Email, err)
	}
	for name, tt := range map[string]struct{ file, want string }{
		"plaintext":   {"notifications: { email: { host: smtp, transport: none, from: q@example.com } }", `notifications.email.transport must be starttls or tls, not "none"`},
		"no host":     {"notifications: { email: { from: q@example.com } }", "notifications.email.host must be a host name"},
		"port":        {"notifications: { email: { host: smtp, port: 70000, from: q@example.com } }", "notifications.email.port must be 1 to 65535"},
		"from":        {"notifications: { email: { host: smtp, from: quai } }", `notifications.email.from: "quai" is not an email address`},
		"from lines":  {"notifications: { email: { host: smtp, from: \"Quai\\r\\nBcc: x@example.com <q@example.com>\" } }", "must be one line"},
		"password":    {"notifications: { email: { host: smtp, from: q@example.com, username: quai, password: { ref: hunter2 } } }", "notifications.email.password.ref must be a secret reference"},
		"no username": {"notifications: { email: { host: smtp, from: q@example.com, password: { ref: env:SMTP } } }", "notifications.email.password needs notifications.email.username"},
		"public URL":  {"notifications: { email: { host: smtp, from: q@example.com } }", "notifications.email needs server.publicURL"},
		"alert":       {"notifications: { email: { host: smtp, from: q@example.com }, alerts: [\"SecOps <secops@example.com>\"] }", `notifications.alerts[0]: "SecOps <secops@example.com>" must be an address alone`},
		"no email":    {"notifications: { alerts: [secops@example.com] }", "notifications.alerts are email addresses: they need notifications.email"},
	} {
		_, err := Load(write(t, "server: { publicURL: \"\" }\n"+tt.file+"\n"), env(nil))
		if name != "public URL" {
			_, err = Load(write(t, "server: { publicURL: https://quai.example.com }\n"+tt.file+"\n"), env(nil))
		}
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tt.want)
		}
	}
}

// TestNotificationDefaults: what people get until they choose, checked as strictly as their own
// choices.
func TestNotificationDefaults(t *testing.T) {
	c, err := Load("", env(nil))
	if err != nil || *c.Notifications.Defaults.Digest.Hour != 8 || c.Notifications.Defaults.Digest.TimeZone != "UTC" || len(c.Notifications.Channels()) != 0 {
		t.Errorf("default digest = %+v, %v", c.Notifications.Defaults, err)
	}
	const email = "server: { publicURL: https://quai.example.com }\nnotifications:\n  email: { host: smtp, from: q@example.com }\n"
	c, err = Load(write(t, email+"  defaults: { digest: { hour: 0, timeZone: Europe/Zurich }, channels: { email: { approval_needed: digest, submitted: off, merged: immediate } } }\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if d := c.Notifications.Defaults; *d.Digest.Hour != 0 || d.Digest.TimeZone != "Europe/Zurich" || d.Channels["email"]["submitted"] != ModeOff ||
		!slices.Equal(c.Notifications.Channels(), []string{"email"}) {
		t.Errorf("defaults = %+v", d)
	}
	for name, tt := range map[string]struct{ file, want string }{
		"unknown channel":  {email + "  defaults: { channels: { sms: { submitted: off } } }", `"sms" is not a channel`},
		"not configured":   {"notifications: { defaults: { channels: { email: { submitted: off } } } }", "notifications.defaults.channels.email needs notifications.email"},
		"no teams":         {email + "  defaults: { channels: { teams: { submitted: off } } }", "notifications.defaults.channels.teams needs notifications.teams"},
		"alert":            {email + "  defaults: { channels: { email: { alert: off } } }", "alerts always go at once"},
		"unknown kind":     {email + "  defaults: { channels: { email: { deployed: off } } }", `"deployed" is not a kind of notification`},
		"unknown mode":     {email + "  defaults: { channels: { email: { submitted: weekly } } }", `notifications.defaults.channels.email.submitted must be immediate, digest or off, not "weekly"`},
		"approvals off":    {email + "  defaults: { channels: { email: { approval_needed: off } } }", "approval_needed cannot be off on every channel"},
		"hour":             {email + "  defaults: { digest: { hour: 24 } }", "notifications.defaults.digest.hour must be 0 to 23, not 24"},
		"negative hour":    {email + "  defaults: { digest: { hour: -1 } }", "must be 0 to 23, not -1"},
		"local":            {email + "  defaults: { digest: { timeZone: Local } }", `"Local" is not an IANA time zone name`},
		"path":             {email + "  defaults: { digest: { timeZone: ../../etc/passwd } }", "is not an IANA time zone name"},
		"unknown timezone": {email + "  defaults: { digest: { timeZone: Mars/Olympus_Mons } }", `"Mars/Olympus_Mons" is not a known time zone`},
		"unknown field":    {email + "  defaults: { digest: { minute: 30 } }", "field minute not found"},
	} {
		if _, err := Load(write(t, tt.file+"\n"), env(nil)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tt.want)
		}
	}
}

// TestTeams: channel posts through webhooks that are secret references; a bot with Microsoft's
// public cloud by default, its endpoints https, its secret a reference, and a key for the links
// of its approval cards.
func TestTeams(t *testing.T) {
	const app, tenant = "6a0d3a2e-6a7c-4a43-9b1f-1d1b2c3d4e5f", "8f2a1c3e-0b6d-4e57-9a41-2c7d5e9b0f13"
	p := write(t, `server: { publicURL: https://quai.example.com }
notifications:
  links: { key: { ref: "k8s:quai/chat-links#key" }, previousKeys: [{ ref: "k8s:quai/chat-links#previous" }] }
  teams:
    bot: { appId: `+app+`, tenantId: `+tenant+`, clientSecret: { ref: "k8s:quai/teams#secret" } }
    channels:
      - { name: platform, webhook: { ref: "k8s:quai/teams#platform" }, repositories: [gitops], teams: [platform], events: [approval_needed, alert] }
  defaults: { channels: { teams: { submitted: off } } }
`)
	c, err := Load(p, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	b := c.Notifications.Teams.Bot
	if b.TokenURL != "https://login.microsoftonline.com/"+tenant+"/oauth2/v2.0/token" || b.Scope != DefaultTeamsScope || b.ServiceURL != DefaultTeamsServiceURL ||
		!slices.Equal(c.Notifications.Channels(), []string{"teams"}) || c.Notifications.Links.PreviousKeys[0].Ref != "k8s:quai/chat-links#previous" {
		t.Errorf("teams = %+v, %+v", b, c.Notifications)
	}
	// Channel posts alone reach no person: Teams is not among the channels people choose for.
	c, err = Load(write(t, "server: { publicURL: https://quai.example.com }\nnotifications: { teams: { channels: [{ name: ops, webhook: { ref: env:HOOK }, teams: [ops], events: [merged] }] } }\n"), env(nil))
	if err != nil || len(c.Notifications.Channels()) != 0 {
		t.Errorf("channels alone = %v, %v", c.Notifications.Channels(), err)
	}
	const links = "  links: { key: { ref: env:LINKS } }\n"
	bot := func(extra string) string {
		return links + "  teams: { bot: { appId: " + app + ", tenantId: " + tenant + ", clientSecret: { ref: env:BOT }" + extra + " } }"
	}
	hook := func(extra string) string {
		return "  teams: { channels: [{ name: ops, webhook: { ref: env:HOOK }" + extra + " }] }"
	}
	for name, tt := range map[string]struct{ file, want string }{
		"empty":           {"  teams: {}", "notifications.teams needs a bot, channels, or both"},
		"public URL":      {bot(""), "notifications.teams needs server.publicURL"},
		"app":             {links + "  teams: { bot: { appId: quai, tenantId: " + tenant + ", clientSecret: { ref: env:BOT } } }", `notifications.teams.bot.appId must be the application (client) ID, a GUID, not "quai"`},
		"tenant":          {links + "  teams: { bot: { appId: " + app + ", tenantId: contoso.com, clientSecret: { ref: env:BOT } } }", "notifications.teams.bot.tenantId must be the directory (tenant) ID"},
		"secret":          {links + "  teams: { bot: { appId: " + app + ", tenantId: " + tenant + ", clientSecret: { ref: s3cr3t } } }", "notifications.teams.bot.clientSecret.ref must be a secret reference"},
		"token http":      {bot(", tokenURL: http://login.example.com/token"), "notifications.teams.bot.tokenURL must be an https URL"},
		"service http":    {bot(", serviceURL: http://smba.example.com/teams/"), "notifications.teams.bot.serviceURL must be an https URL"},
		"service creds":   {bot(", serviceURL: \"https://u:p@smba.example.com/\""), "notifications.teams.bot.serviceURL must be an https URL without credentials"},
		"scope":           {bot(", scope: \"openid profile\""), "notifications.teams.bot.scope must be one https scope"},
		"no links":        {"  teams: { bot: { appId: " + app + ", tenantId: " + tenant + ", clientSecret: { ref: env:BOT } } }", "notifications.teams.bot needs notifications.links.key"},
		"links key":       {"  links: { key: { ref: abc } }", "notifications.links.key.ref must be a secret reference"},
		"previous key":    {"  links: { key: { ref: env:K }, previousKeys: [{ ref: abc }] }", "notifications.links.previousKeys[0].ref must be a secret reference"},
		"webhook":         {"  teams: { channels: [{ name: ops, webhook: { ref: \"https://prod.example.com/workflows/x\" }, teams: [ops], events: [merged] }] }", "notifications.teams.channels[0].webhook.ref must be a secret reference"},
		"channel name":    {"  teams: { channels: [{ name: Ops, webhook: { ref: env:HOOK }, teams: [ops], events: [merged] }] }", `notifications.teams.channels[0].name must be lowercase letters, digits and dashes, not "Ops"`},
		"channel twice":   {"  teams: { channels: [{ name: ops, webhook: { ref: env:A }, teams: [ops], events: [merged] }, { name: ops, webhook: { ref: env:B }, teams: [ops], events: [merged] }] }", "notifications.teams.channels[1].name: ops names another channel already"},
		"nobody's events": {hook(", events: [merged]"), "notifications.teams.channels[0] needs repositories, teams, or both"},
		"no events":       {hook(", teams: [ops]"), "notifications.teams.channels[0].events: name at least one"},
		"unknown event":   {hook(", teams: [ops], events: [deployed]"), `notifications.teams.channels[0].events: "deployed" is not an event`},
	} {
		head := "server: { publicURL: https://quai.example.com }\nnotifications:\n"
		if name == "public URL" {
			head = "server: { publicURL: \"\" }\nnotifications:\n"
		}
		if _, err := Load(write(t, head+tt.file+"\n"), env(nil)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tt.want)
		}
	}
}

// TestSlack: a bot token and an optional signing secret that are secret references, Slack's Web
// API by default and https, channels by ID, and a key for the links of approval messages. Slack
// reaches people: it is among the channels they choose for.
func TestSlack(t *testing.T) {
	c, err := Load(write(t, `server: { publicURL: https://quai.example.com }
notifications:
  links: { key: { ref: "k8s:quai/chat-links#key" } }
  slack:
    botToken: { ref: "k8s:quai/slack#bot-token" }
    signingSecret: { ref: "k8s:quai/slack#signing-secret" }
    channels:
      - { name: platform, channel: C0123ABCDEF, repositories: [gitops], teams: [platform], events: [approval_needed, alert] }
  defaults: { channels: { slack: { submitted: off } } }
`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if s := c.Notifications.Slack; s.APIURL != DefaultSlackAPIURL || s.BotToken.Ref != "k8s:quai/slack#bot-token" || s.Channels[0].Channel != "C0123ABCDEF" ||
		!slices.Equal(c.Notifications.Channels(), []string{"slack"}) {
		t.Errorf("slack = %+v, channels %v", s, c.Notifications.Channels())
	}
	// Without a signing secret: no interaction endpoint, nothing else changes.
	if _, err := Load(write(t, "server: { publicURL: https://quai.example.com }\nnotifications:\n  links: { key: { ref: env:LINKS } }\n  slack: { botToken: { ref: env:BOT } }\n"), env(nil)); err != nil {
		t.Errorf("no signing secret: %v", err)
	}
	const links = "  links: { key: { ref: env:LINKS } }\n"
	slack := func(extra string) string { return links + "  slack: { botToken: { ref: env:BOT }" + extra + " }" }
	channel := func(extra string) string {
		return slack(", channels: [{ name: ops, channel: C0123ABCDEF" + extra + " }]")
	}
	for name, tt := range map[string]struct{ file, want string }{
		"public URL":      {slack(""), "notifications.slack needs server.publicURL"},
		"no token":        {links + "  slack: {}", "notifications.slack.botToken.ref must be a secret reference"},
		"token":           {links + "  slack: { botToken: { ref: xoxb-1234-5678-abcdef } }", "notifications.slack.botToken.ref must be a secret reference"},
		"signing secret":  {slack(", signingSecret: { ref: 8f742231b10e8888abcd99yyyzzz85a5 }"), "notifications.slack.signingSecret.ref must be a secret reference"},
		"api http":        {slack(", apiURL: http://slack.com/api"), "notifications.slack.apiURL must be an https URL"},
		"api creds":       {slack(", apiURL: \"https://u:p@slack.example.com/api\""), "notifications.slack.apiURL must be an https URL without credentials"},
		"api query":       {slack(", apiURL: \"https://slack.example.com/api?token=x\""), "notifications.slack.apiURL must be an https URL without credentials or a query"},
		"no links":        {"  slack: { botToken: { ref: env:BOT } }", "notifications.slack needs notifications.links.key"},
		"channel name":    {slack(", channels: [{ name: Ops, channel: C0123ABCDEF, teams: [ops], events: [merged] }]"), `notifications.slack.channels[0].name must be lowercase letters, digits and dashes, not "Ops"`},
		"channel twice":   {slack(", channels: [{ name: ops, channel: C0123ABCDEF, teams: [ops], events: [merged] }, { name: ops, channel: C0123ABCDEG, teams: [ops], events: [merged] }]"), "notifications.slack.channels[1].name: ops names another channel already"},
		"channel by name": {slack(", channels: [{ name: ops, channel: \"#ops\", teams: [ops], events: [merged] }]"), `notifications.slack.channels[0].channel must be the Slack channel's ID, such as C0123ABCDEF (its details in Slack show it), not "#ops"`},
		"user channel":    {slack(", channels: [{ name: ops, channel: U0123ABCDEF, teams: [ops], events: [merged] }]"), "notifications.slack.channels[0].channel must be the Slack channel's ID"},
		"nobody's events": {channel(", events: [merged]"), "notifications.slack.channels[0] needs repositories, teams, or both"},
		"no events":       {channel(", teams: [ops]"), "notifications.slack.channels[0].events: name at least one"},
		"unknown event":   {channel(", teams: [ops], events: [deployed]"), `notifications.slack.channels[0].events: "deployed" is not an event`},
		"defaults":        {"  defaults: { channels: { slack: { submitted: off } } }", "notifications.defaults.channels.slack needs notifications.slack"},
	} {
		head := "server: { publicURL: https://quai.example.com }\nnotifications:\n"
		if name == "public URL" {
			head = "server: { publicURL: \"\" }\nnotifications:\n"
		}
		if _, err := Load(write(t, head+tt.file+"\n"), env(nil)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tt.want)
		}
	}
}

// TestTimeZone: an IANA name, never a path, Local or an empty name.
func TestTimeZone(t *testing.T) {
	for _, name := range []string{"UTC", "Europe/Zurich", "America/Argentina/Buenos_Aires", "Etc/GMT+5", "Etc/GMT-14", "America/Port-au-Prince", "Asia/Kolkata"} {
		if loc, err := TimeZone(name); err != nil || loc.String() != name {
			t.Errorf("TimeZone(%q) = %v, %v", name, loc, err)
		}
	}
	for _, name := range []string{"", "Local", "local", "/etc/localtime", "../etc/passwd", "Europe/../../etc/passwd", "Europe/Zurich/", "Europe//Zurich",
		"./UTC", "Europe\\Zurich", "Europe/Zurich\x00", "Europe/Zurich ", "zone1970.tab", "Mars/Olympus_Mons", "A/B/C/D", strings.Repeat("A", 40)} {
		if loc, err := TimeZone(name); err == nil {
			t.Errorf("TimeZone(%q) = %v, want an error", name, loc)
		}
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
  teams:
    - { group: sg-quai-*, team: "*" }
    - { group: Platform Engineers, team: platform }
`)
	c, err := Load(p, env(map[string]string{"QUAI_AUTH_OIDC_CLIENT_SECRET": "s3cret"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.OIDC.ClientSecret != "s3cret" || c.Auth.OIDC.GroupsClaim != "roles" || len(c.Auth.Roles) != 2 || len(c.Auth.Dev.Users) != 0 ||
		len(c.Auth.Teams) != 2 || c.Auth.Teams[0] != (TeamMapping{Group: "sg-quai-*", Team: "*"}) {
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
		"team without a group":      {file: "auth: { teams: [{ team: shop }] }\n", want: "auth.teams[0] needs a group and a team"},
		"group without a team":      {file: "auth: { teams: [{ group: sg-shop }] }\n", want: "auth.teams[0] needs a group and a team"},
		"two stars":                 {file: "auth: { teams: [{ group: \"sg-*-*\", team: \"*\" }] }\n", want: "may hold one * at most"},
		"a star the group lacks":    {file: "auth: { teams: [{ group: sg-shop, team: \"*\" }] }\n", want: "only when its group has one"},
		"nameless dev user":         {file: "auth: { dev: { users: [{ groups: [a] }] } }\n", want: "every user needs a name"},
		"negative session":          {file: "auth: { session: { idleTimeout: -1m } }\n", want: "must not be negative"},
		"step-up under a second":    {file: "auth: { stepUp: { maxAge: 500ms } }\n", want: "auth.stepUp.maxAge must be at least 1s"},
		"negative step-up":          {file: "auth: { stepUp: { maxAge: -5m } }\n", want: "auth.stepUp.maxAge must be at least 1s"},
		"an empty acr":              {env: oidcEnv, file: "auth: { stepUp: { acrValues: [\"\"] } }\n", want: "auth.stepUp.acrValues[0] must be a non-empty value"},
		"an acr with a space":       {env: oidcEnv, file: "auth: { stepUp: { acrValues: [\"a b\"] } }\n", want: "without spaces"},
		"a bad context":             {env: oidcEnv, file: "auth: { stepUp: { authContext: c100 } }\n", want: "c1 to c99"},
		"a context named":           {env: oidcEnv, file: "auth: { stepUp: { authContext: mfa } }\n", want: "c1 to c99"},
		"acr values in dev":         {file: "auth: { stepUp: { acrValues: [mfa] } }\n", want: "need auth.mode oidc"},
		"a context in dev":          {file: "auth: { stepUp: { authContext: c1 } }\n", want: "need auth.mode oidc"},
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

// oidcEnv configures OIDC login.
var oidcEnv = map[string]string{"QUAI_AUTH_MODE": "oidc", "QUAI_AUTH_OIDC_ISSUER": "https://idp", "QUAI_AUTH_OIDC_CLIENT_ID": "q", "QUAI_AUTH_OIDC_REDIRECT_URL": "https://q/cb"}

// TestStepUp: auth.stepUp is optional; when set, its maxAge defaults to five minutes (open
// question 49).
func TestStepUp(t *testing.T) {
	c, err := Load("", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.StepUp != nil || c.Auth.StepUpSettings().MaxAge != DefaultStepUpMaxAge || DefaultStepUpMaxAge != 5*time.Minute {
		t.Errorf("no step-up = %+v, %+v", c.Auth.StepUp, c.Auth.StepUpSettings())
	}
	p := write(t, "auth: { stepUp: { admin: true } }\n")
	if c, err = Load(p, env(nil)); err != nil {
		t.Fatal(err)
	}
	if s := c.Auth.StepUpSettings(); c.Auth.StepUp == nil || !s.Admin || s.MaxAge != 5*time.Minute {
		t.Errorf("step-up without maxAge = %+v", s)
	}
	p = write(t, "auth: { stepUp: { maxAge: 10m, acrValues: [\"http://schemas.openid.net/pape/policies/2007/06/multi-factor\", mfa], authContext: c42 } }\n")
	if c, err = Load(p, env(oidcEnv)); err != nil {
		t.Fatal(err)
	}
	if s := c.Auth.StepUpSettings(); s.MaxAge != 10*time.Minute || len(s.ACRValues) != 2 || s.AuthContext != "c42" || s.Admin {
		t.Errorf("step-up = %+v", s)
	}
}

// TestGraph: auth.graph and auth.oidc.managerClaim (open questions 68 and 69): Graph's root
// defaults to the global service, the scopes a login asks for follow what is read, and what would
// make Quai call another host, read groups it cannot match, or take the manager from two places
// is refused.
func TestGraph(t *testing.T) {
	const tenant = "8f2a1c3e-0b6d-4e57-9a41-2c7d5e9b0f13"
	c, err := Load(write(t, "auth: { graph: { tenantId: "+tenant+", manager: true, groups: true } }\n"), env(oidcEnv))
	if err != nil {
		t.Fatal(err)
	}
	if g := c.Auth.Graph; g.BaseURL != DefaultGraphBaseURL || DefaultGraphBaseURL != "https://graph.microsoft.com" ||
		!slices.Equal(g.GraphScopes(), []string{"https://graph.microsoft.com/User.Read", "https://graph.microsoft.com/User.Read.All"}) {
		t.Errorf("graph = %+v, scopes %v", g, g.GraphScopes())
	}
	c, err = Load(write(t, "auth: { oidc: { managerClaim: manager_oid }, graph: { tenantId: "+tenant+", baseURL: \"https://graph.microsoft.us/\", groups: true } }\n"), env(oidcEnv))
	if err != nil {
		t.Fatal(err)
	}
	if g := c.Auth.Graph; g.BaseURL != "https://graph.microsoft.us" || !slices.Equal(g.GraphScopes(), []string{"https://graph.microsoft.us/User.Read"}) || c.Auth.OIDC.ManagerClaim != "manager_oid" {
		t.Errorf("graph = %+v, scopes %v, %+v", g, g.GraphScopes(), c.Auth.OIDC)
	}
	if c, err = Load("", env(oidcEnv)); err != nil || c.Auth.Graph != nil || c.Auth.Graph.GraphScopes() != nil {
		t.Errorf("no graph = %+v, %v", c.Auth.Graph, err)
	}
	g := "auth: { graph: { tenantId: " + tenant + ", manager: true, "
	for name, tt := range map[string]struct {
		env  map[string]string
		file string
		want string
	}{
		"graph in dev":         {file: g + "} }\n", want: "auth.graph needs auth.mode oidc"},
		"a tenant name":        {env: oidcEnv, file: "auth: { graph: { tenantId: contoso.onmicrosoft.com, manager: true } }\n", want: "auth.graph.tenantId must be the directory (tenant) ID"},
		"an http root":         {env: oidcEnv, file: g + "baseURL: \"http://graph.microsoft.com\" } }\n", want: "auth.graph.baseURL must be Microsoft Graph's https root"},
		"a path":               {env: oidcEnv, file: g + "baseURL: \"https://graph.microsoft.com/v1.0\" } }\n", want: "auth.graph.baseURL must be"},
		"a query":              {env: oidcEnv, file: g + "baseURL: \"https://graph.microsoft.com?x=1\" } }\n", want: "auth.graph.baseURL must be"},
		"credentials":          {env: oidcEnv, file: g + "baseURL: \"https://u:p@graph.microsoft.com\" } }\n", want: "auth.graph.baseURL must be"},
		"nothing to read":      {env: oidcEnv, file: "auth: { graph: { tenantId: " + tenant + " } }\n", want: "auth.graph needs manager or groups"},
		"two manager sources":  {env: oidcEnv, file: "auth: { oidc: { managerClaim: m }, graph: { tenantId: " + tenant + ", manager: true } }\n", want: "keep one"},
		"groups from roles":    {env: oidcEnv, file: "auth: { oidc: { groupsClaim: roles }, graph: { tenantId: " + tenant + ", groups: true } }\n", want: "auth.graph.groups needs auth.oidc.groupsClaim groups"},
		"another resource":     {env: oidcEnv, file: "auth: { oidc: { scopes: [openid, \"api://quai/read\"] }, graph: { tenantId: " + tenant + ", manager: true } }\n", want: `not "api://quai/read"`},
		"a manager claim, dev": {file: "auth: { oidc: { managerClaim: manager } }\n", want: "auth.oidc.managerClaim must be a claim's name and needs auth.mode oidc"},
		"a claim with a space": {env: oidcEnv, file: "auth: { oidc: { managerClaim: \"a b\" } }\n", want: "auth.oidc.managerClaim must be a claim's name"},
	} {
		if _, err := Load(write(t, tt.file), env(tt.env)); err == nil || !strings.Contains(err.Error(), tt.want) {
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

// TestApprovals: approvals.reminders and approvals.escalation (open question 25): off by default;
// an escalation needs both when and whom, comes after the reminder, and names groups or users.
func TestApprovals(t *testing.T) {
	c, err := Load("", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Approvals.Reminders.After != 0 || c.Approvals.Escalation.After != 0 || len(c.Approvals.Escalation.To) != 0 {
		t.Errorf("default approvals = %+v", c.Approvals)
	}
	p := write(t, "approvals: { reminders: { after: 24h }, escalation: { after: 72h, to: [group:platform-leads, user:dana.smith@example.com] } }\n")
	if c, err = Load(p, env(nil)); err != nil {
		t.Fatal(err)
	}
	if a := c.Approvals; a.Reminders.After != 24*time.Hour || a.Escalation.After != 72*time.Hour || !slices.Equal(a.Escalation.To, []string{"group:platform-leads", "user:dana.smith@example.com"}) {
		t.Errorf("approvals = %+v", a)
	}
	for yaml, want := range map[string]string{
		"approvals: { reminders: { after: -1h } }":                                           "approvals.reminders.after must not be negative",
		"approvals: { escalation: { after: 48h } }":                                          "approvals.escalation.after needs approvals.escalation.to",
		"approvals: { escalation: { to: [group:leads] } }":                                   "approvals.escalation.to needs approvals.escalation.after",
		"approvals: { reminders: { after: 48h }, escalation: { after: 48h, to: [user:x] } }": "must come after approvals.reminders.after",
		"approvals: { escalation: { after: 48h, to: [leads] } }":                             `approvals.escalation.to[0] must be group:<name> or user:<name>, not "leads"`,
		"approvals: { escalation: { after: 48h, to: [\"group:\"] } }":                        "approvals.escalation.to[0] must be",
	} {
		if _, err := Load(write(t, yaml+"\n"), env(nil)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", yaml, err, want)
		}
	}
}
