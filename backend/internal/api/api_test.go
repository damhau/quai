package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

func newTest(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	return New(Options{Version: "v1.2.3", Env: "development", Logger: slog.New(slog.NewJSONHandler(&logs, nil))}), &logs
}

func get(h http.Handler, path string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthAndMeta(t *testing.T) {
	h, logs := newTest(t)
	if rec := get(h, "/healthz"); rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("/healthz = %d %q", rec.Code, rec.Body)
	}
	rec := get(h, Prefix+"/meta", "X-Request-ID", "abc-123")
	var meta struct{ Version, Env string }
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil || rec.Code != http.StatusOK || meta.Version != "v1.2.3" || meta.Env != "development" {
		t.Errorf("meta = %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Request-ID") != "abc-123" {
		t.Errorf("request ID = %q, want the caller's", rec.Header().Get("X-Request-ID"))
	}
	var line map[string]any
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &line); err != nil {
		t.Fatal(err)
	}
	if line["msg"] != "request" || line["request_id"] != "abc-123" || line["path"] != Prefix+"/meta" || line["status"] != 200.0 || line["method"] != "GET" {
		t.Errorf("log line = %v", line)
	}
	// An unsafe request ID is replaced.
	if id := get(h, "/healthz", "X-Request-ID", "bad id\n").Header().Get("X-Request-ID"); len(id) != 16 {
		t.Errorf("replaced request ID = %q", id)
	}
}

func TestOpenAPI(t *testing.T) {
	h, _ := newTest(t)
	rec := get(h, Prefix+"/openapi.json")
	var doc struct {
		OpenAPI string
		Info    struct{ Title, Version string }
		Paths   map[string]any
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("openapi = %d, %v", rec.Code, err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") || doc.Info.Version != "v1.2.3" || doc.Paths[Prefix+"/meta"] == nil {
		t.Errorf("openapi = %+v", doc)
	}
	if rec := get(h, "/docs"); rec.Code != http.StatusNotFound {
		t.Errorf("/docs = %d, want no docs page", rec.Code)
	}
}

func TestProblems(t *testing.T) {
	h, logs := newTest(t)
	for path, status := range map[string]int{Prefix + "/nope": 404, "/elsewhere": 404} {
		rec := get(h, path)
		var p struct {
			Title  string
			Status int
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || rec.Code != status || p.Status != status ||
			rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s = %d %s %q", path, rec.Code, rec.Body, rec.Header().Get("Content-Type"))
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Errorf("POST /healthz = %d", rec.Code)
	}

	// A panic is a 500 problem and an error log line, without the panic's message in the body.
	panicky := recoverer(slog.New(slog.NewJSONHandler(logs, nil)))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret detail")
	}))
	rec = httptest.NewRecorder()
	panicky.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") || !strings.Contains(logs.String(), `"panic":"secret detail"`) {
		t.Errorf("panic = %d %s", rec.Code, rec.Body)
	}
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Error("ErrAbortHandler was swallowed")
		}
	}()
	recoverer(slog.Default())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestProblemMapping(t *testing.T) {
	ve := &schema.ValidationError{Fields: map[string]string{"cpu": "must be at most 32", "$": "must be an object"}}
	p := Problem(fmt.Errorf("plan: %w", ve), "body.document")
	if p.GetStatus() != 422 {
		t.Fatalf("status = %d", p.GetStatus())
	}
	b, _ := json.Marshal(p)
	var got struct {
		Errors []struct{ Location, Message string }
	}
	if err := json.Unmarshal(b, &got); err != nil || len(got.Errors) != 2 ||
		got.Errors[0].Location != "body.document" || got.Errors[0].Message != "must be an object" ||
		got.Errors[1].Location != "body.document.cpu" || got.Errors[1].Message != "must be at most 32" {
		t.Errorf("problem = %s", b)
	}
	for err, status := range map[error]int{
		fmt.Errorf("vm/prod/x: %w", plan.ErrNotFound): 404,
		fmt.Errorf("vm/prod/x: %w", plan.ErrExists):   409,
		plan.ErrNoChange:               422,
		errors.New("database is down"): 500,
	} {
		p := Problem(err, "body")
		b, _ := json.Marshal(p)
		if p.GetStatus() != status || status == 500 && strings.Contains(string(b), "database") {
			t.Errorf("Problem(%v) = %d %s", err, p.GetStatus(), b)
		}
	}
}
