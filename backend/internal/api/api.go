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
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Prefix is where the REST API lives.
const Prefix = "/api/v1"

// Options configures the API.
type Options struct {
	Version string
	Env     string
	Logger  *slog.Logger
}

// New returns the HTTP handler of the API.
func New(opts Options) http.Handler {
	r := chi.NewRouter()
	r.Use(requestID, accessLog(opts.Logger), recoverer(opts.Logger))
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeProblem(w, http.StatusNotFound, "no such resource")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeProblem(w, http.StatusMethodNotAllowed, "this method is not allowed on this resource")
	})
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

	return otelhttp.NewHandler(r, "quai", otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
		return r.Method + " " + r.URL.Path
	}))
}

// MetaOutput describes the running server.
type MetaOutput struct {
	Body struct {
		Version string `json:"version" doc:"The server's version"`
		Env     string `json:"env" enum:"development,production" doc:"The server's environment"`
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
