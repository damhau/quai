package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandler(t *testing.T) {
	files := fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>Quai</title>")},
		"favicon.svg":          {Data: []byte("<svg/>")},
		"assets/index-ab12.js": {Data: []byte("console.log(1)")},
	}
	h := Handler(files)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	for _, tc := range []struct {
		path, body, cache string
		status            int
	}{
		{"/", "<title>Quai</title>", "no-cache", 200},
		{"/requests/42", "<title>Quai</title>", "no-cache", 200}, // a page of the app
		{"/index.html", "<title>Quai</title>", "no-cache", 200},
		{"/assets/index-ab12.js", "console.log(1)", "public, max-age=31536000, immutable", 200},
		{"/favicon.svg", "<svg/>", "no-cache", 200},
		{"/assets/index-old.js", "", "", 404}, // not the page in place of a script
		{"/../../etc/passwd", "<title>Quai</title>", "no-cache", 200},
	} {
		rec := get(tc.path)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.body) || rec.Header().Get("Cache-Control") != tc.cache {
			t.Errorf("%s = %d %q, cache %q", tc.path, rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self';") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", tc.path, csp)
		}
	}
}

func TestFilesWithoutABuild(t *testing.T) {
	// The repository holds no build: go test and go vet work without one, and the server then
	// serves the API alone.
	if Files() != nil {
		t.Skip("this binary embeds a build of the frontend")
	}
}
