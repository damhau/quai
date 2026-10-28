package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWebhookHost: with a webhook host name, /webhooks/ answers there only, and that host answers
// nothing else but run reports and /healthz.
func TestWebhookHost(t *testing.T) {
	hooks := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	h := New(Options{Version: "test", Logger: slog.New(slog.DiscardHandler), Webhooks: hooks, WebhookHost: "hooks.quai.example.com"})
	for _, tc := range []struct {
		host, method, path string
		want               int
	}{
		{"hooks.quai.example.com", http.MethodPost, "/webhooks/infra-live", http.StatusAccepted},
		{"hooks.quai.example.com:8443", http.MethodPost, "/webhooks/infra-live", http.StatusAccepted},
		{"quai.example.com", http.MethodPost, "/webhooks/infra-live", http.StatusNotFound},
		{"hooks.quai.example.com", http.MethodGet, "/healthz", http.StatusOK},
		{"hooks.quai.example.com", http.MethodGet, Prefix + "/meta", http.StatusNotFound},
		{"quai.example.com", http.MethodGet, Prefix + "/meta", http.StatusOK},
	} {
		req := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s%s = %d, want %d", tc.method, tc.host, tc.path, rec.Code, tc.want)
		}
	}
	// Without a webhook host, both answer everywhere.
	h = New(Options{Version: "test", Logger: slog.New(slog.DiscardHandler), Webhooks: hooks})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://quai.example.com/webhooks/infra-live", nil))
	if rec.Code != http.StatusAccepted {
		t.Errorf("no webhook host: %d", rec.Code)
	}
}
