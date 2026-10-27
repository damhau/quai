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
	Env       string    `yaml:"env"`
	Server    Server    `yaml:"server"`
	Log       Log       `yaml:"log"`
	Telemetry Telemetry `yaml:"telemetry"`
}

// Server configures the HTTP server.
type Server struct {
	Addr            string        `yaml:"addr"`            // listen address, such as :8080
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"` // how long requests in flight may finish
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
	{"QUAI_LOG_LEVEL", func(c *Config, v string) error { c.Log.Level = v; return nil }},
	{"QUAI_LOG_FORMAT", func(c *Config, v string) error { c.Log.Format = v; return nil }},
	{"QUAI_TELEMETRY_OTLP_ENDPOINT", func(c *Config, v string) error { c.Telemetry.OTLPEndpoint = v; return nil }},
	{"QUAI_TELEMETRY_SERVICE_NAME", func(c *Config, v string) error { c.Telemetry.ServiceName = v; return nil }},
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
}

func (c *Config) validate() error {
	var errs []error
	if c.Env != Development && c.Env != Production {
		errs = append(errs, fmt.Errorf("env must be %s or %s, not %q", Development, Production, c.Env))
	}
	if _, port, err := net.SplitHostPort(c.Server.Addr); err != nil || port == "" {
		errs = append(errs, fmt.Errorf("server.addr must be host:port or :port, not %q", c.Server.Addr))
	}
	if c.Server.ShutdownTimeout < 0 {
		errs = append(errs, errors.New("server.shutdownTimeout must not be negative"))
	}
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
