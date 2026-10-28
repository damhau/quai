// Package api serves Quai's HTTP API: REST under /api/v1, described by an OpenAPI 3.1 document
// generated from the handlers by Huma, with RFC 9457 problem details for every error
// (docs/design/11-integrations.md#api). It also serves /healthz for liveness probes.
//
// Every request gets an ID (X-Request-ID, kept from the caller when it is safe), one log line,
// a trace span, and recovery from panics.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/web"
)

// Prefix is where the REST API lives.
const Prefix = "/api/v1"

// Options configures the API.
type Options struct {
	Version string
	Env     string
	Logger  *slog.Logger
	// Auth and Store are nil when the server runs without a database: nobody can sign in, and
	// only /healthz and /api/v1/meta answer.
	Auth  *auth.Authenticator
	Store *store.Store
	// Repositories are the connected repositories; nil when none are configured.
	Repositories *repositories.Registry
	// Requests runs requests; nil without repositories or a database.
	Requests *requests.Service
	// UI is the frontend's build, served at every path the API and /auth do not answer; nil when
	// the binary was built without it.
	UI fs.FS
	// Webhooks receives the platforms' deliveries under /webhooks/; nil without a database.
	Webhooks http.Handler
	// WebhookHost, when set, is the only host name /webhooks/ answers on, and that host answers
	// nothing but /webhooks/, the run reports and /healthz: it can be exposed to the platforms
	// without exposing the rest.
	WebhookHost string
}

// New returns the HTTP handler of the API.
func New(opts Options) http.Handler {
	r, _ := build(opts, false)
	var h http.Handler = r
	if opts.WebhookHost != "" {
		h = webhookHost(opts.WebhookHost, r)
	}
	return otelhttp.NewHandler(h, "quai", otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
		return r.Method + " " + r.URL.Path
	}))
}

// Document returns the OpenAPI 3.1 document of every operation, whatever the server's
// configuration: the frontend generates its client from it.
func Document(version string) ([]byte, error) {
	_, a := build(Options{Version: version, Logger: slog.New(slog.DiscardHandler)}, true)
	return json.MarshalIndent(a.OpenAPI(), "", "  ")
}

// build makes the router and registers the operations whose dependencies opts has, or all of them.
func build(opts Options, all bool) (*chi.Mux, huma.API) {
	r := chi.NewRouter()
	r.Use(requestID, accessLog(opts.Logger), recoverer(opts.Logger))
	if opts.Auth != nil {
		r.Use(opts.Auth.Middleware)
		r.Mount("/auth", opts.Auth.Routes())
	}
	var ui http.Handler
	if opts.UI != nil {
		ui = web.Handler(opts.UI)
	}
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		page := req.Method == http.MethodGet || req.Method == http.MethodHead
		if ui != nil && page && !strings.HasPrefix(req.URL.Path, Prefix+"/") && !strings.HasPrefix(req.URL.Path, "/auth/") {
			ui.ServeHTTP(w, req)
			return
		}
		writeProblem(w, http.StatusNotFound, "no such resource")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeProblem(w, http.StatusMethodNotAllowed, "this method is not allowed on this resource")
	})
	if opts.Webhooks != nil {
		r.Mount(auth.WebhooksPath, opts.Webhooks)
	}
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	cfg := huma.DefaultConfig("Quai API", opts.Version)
	cfg.Info.Description = "Governed self-service in front of infrastructure as code."
	cfg.OpenAPIPath = Prefix + "/openapi"
	cfg.SchemasPath = Prefix + "/schemas"
	cfg.DocsPath = "" // the docs page loads scripts from a CDN; the OpenAPI document is enough
	a := humachi.New(r, cfg)
	registerMeta(a, opts)
	if all || opts.Store != nil {
		registerIdentity(a, opts.Store)
	}
	if all || opts.Repositories != nil {
		registerCatalog(a, opts.Repositories, opts.Store)
		registerPreview(a, opts.Repositories)
	}
	if all || opts.Requests != nil {
		registerRequests(a, opts.Requests, opts.Store, opts.Logger)
		registerHistory(a, opts.Repositories, opts.Store)
	}
	return r, a
}

// webhookHost keeps the webhooks on their own host name, and that host to the webhooks.
func webhookHost(host string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			name = h
		}
		onHost := strings.EqualFold(name, host)
		hook := strings.HasPrefix(r.URL.Path, auth.WebhooksPath)
		if hook != onHost && !(onHost && (r.URL.Path == auth.RunsPath || r.URL.Path == "/healthz")) {
			writeProblem(w, http.StatusNotFound, "no such resource")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// MetaOutput describes the running server.
type MetaOutput struct {
	Body struct {
		Version string `json:"version" doc:"The server's version"`
		Env     string `json:"env" enum:"development,production" doc:"The server's environment"`
		Auth    string `json:"auth" enum:"oidc,dev,none" doc:"How people sign in: OIDC login, the development switcher, or not at all (no database)"`
	}
}

func registerMeta(a huma.API, opts Options) {
	// authz: public — the version and environment, for clients and probes.
	huma.Register(a, huma.Operation{
		OperationID: "get-meta",
		Method:      http.MethodGet,
		Path:        Prefix + "/meta",
		Summary:     "Describe the server",
		Tags:        []string{"Meta"},
	}, func(context.Context, *struct{}) (*MetaOutput, error) {
		out := &MetaOutput{}
		out.Body.Version = opts.Version
		out.Body.Env = opts.Env
		out.Body.Auth = "none"
		if opts.Auth != nil {
			out.Body.Auth = opts.Auth.Mode()
		}
		return out, nil
	})
}

type ctxKey struct{}

var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RequestID returns the ID of the request ctx belongs to.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// requestID keeps the caller's X-Request-ID when it is short and plain, and makes one otherwise.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !safeID.MatchString(id) {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (rec *recorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += n
	return n, err
}

func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// accessLog writes one line per request. It never logs bodies, query strings or headers, which
// may carry tokens.
func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			level := slog.LevelInfo
			if rec.status >= 500 {
				level = slog.LevelError
			}
			log.LogAttrs(r.Context(), level, "request",
				slog.String("request_id", RequestID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}

// recoverer turns a panic in a handler into a 500 problem and an error log line.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler {
						panic(v)
					}
					log.ErrorContext(r.Context(), "panic", slog.String("request_id", RequestID(r.Context())), slog.Any("panic", v))
					writeProblem(w, http.StatusInternalServerError, "")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// writeProblem writes an RFC 9457 problem outside Huma's handlers.
func writeProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(huma.ErrorModel{Title: http.StatusText(status), Status: status, Detail: detail})
}
