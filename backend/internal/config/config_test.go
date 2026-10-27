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
	prod, err := Load("", env(map[string]string{"QUAI_ENV": "production"}))
	if err != nil || prod.Log.Format != "json" {
		t.Errorf("production defaults = %+v, %v", prod, err)
	}
}

func TestFileAndEnvironment(t *testing.T) {
	p := write(t, "env: production\nserver: { addr: \"127.0.0.1:9000\", shutdownTimeout: 5s }\nlog: { level: debug, format: text }\ntelemetry: { otlpEndpoint: \"http://collector:4318\" }\n")
	c, err := Load(p, env(map[string]string{"QUAI_LOG_LEVEL": "warn", "QUAI_SERVER_SHUTDOWN_TIMEOUT": "1m", "QUAI_TELEMETRY_SERVICE_NAME": "quai-test"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != Production || c.Server.Addr != "127.0.0.1:9000" || c.Server.ShutdownTimeout != time.Minute ||
		c.Log.Level != "warn" || c.Log.Format != "text" || c.Telemetry.OTLPEndpoint != "http://collector:4318" || c.Telemetry.ServiceName != "quai-test" {
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
		"level":            {env: map[string]string{"QUAI_LOG_LEVEL": "loud"}, want: `log.level must be debug, info, warn or error, not "loud"`},
		"format":           {env: map[string]string{"QUAI_LOG_FORMAT": "xml"}, want: `log.format must be json or text`},
		"endpoint":         {env: map[string]string{"QUAI_TELEMETRY_OTLP_ENDPOINT": "collector:4318"}, want: "telemetry.otlpEndpoint must be an http or https URL"},
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
