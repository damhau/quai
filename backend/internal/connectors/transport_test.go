package connectors

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a fake clock: sleeping moves it forward.
type clock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.sleeps = append(c.sleeps, d)
	return nil
}

// platform answers with the responses of script in turn, then 200, recording what it received.
type platform struct {
	*httptest.Server
	mu     sync.Mutex
	script []func(w http.ResponseWriter)
	bodies []string
}

func newPlatform(t *testing.T, script ...func(w http.ResponseWriter)) *platform {
	p := &platform{script: script}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.bodies = append(p.bodies, string(b))
		var next func(http.ResponseWriter)
		if len(p.script) > 0 {
			next, p.script = p.script[0], p.script[1:]
		}
		p.mu.Unlock()
		if next != nil {
			next(w)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *platform) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.bodies)
}

func reply(status int, header ...string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(status)
	}
}

func client(log *slog.Logger) (*http.Client, *Transport, *clock) {
	c := &clock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	tr := NewTransport(http.DefaultTransport, log)
	tr.now, tr.sleep = c.Now, c.Sleep
	return &http.Client{Transport: tr}, tr, c
}

func send(t *testing.T, c *http.Client, method, u, body string) (int, error) {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

func TestRetryAfter(t *testing.T) {
	p := newPlatform(t, reply(http.StatusTooManyRequests, "Retry-After", "2"))
	c, _, clk := client(nil)
	if status, err := send(t, c, http.MethodPost, p.URL, "payload"); err != nil || status != http.StatusOK {
		t.Fatalf("status %d, %v", status, err)
	}
	if p.calls() != 2 || p.bodies[1] != "payload" || len(clk.sleeps) != 1 || clk.sleeps[0] != 2*time.Second {
		t.Errorf("calls %d %q, sleeps %v", p.calls(), p.bodies, clk.sleeps)
	}

	// As an HTTP date, on a 503 for an idempotent call.
	at := clk.Now().Add(5 * time.Second).Format(http.TimeFormat)
	p.script = []func(http.ResponseWriter){reply(http.StatusServiceUnavailable, "Retry-After", at)}
	if status, err := send(t, c, http.MethodGet, p.URL, ""); err != nil || status != http.StatusOK || clk.sleeps[1] != 5*time.Second {
		t.Errorf("503 on GET: %d, %v, sleeps %v", status, err, clk.sleeps)
	}
	// A 503 on a POST may have done something: it is returned, not retried.
	p.script = []func(http.ResponseWriter){reply(http.StatusServiceUnavailable, "Retry-After", "1")}
	if status, _ := send(t, c, http.MethodPost, p.URL, "x"); status != http.StatusServiceUnavailable {
		t.Errorf("503 on POST = %d", status)
	}
}

func TestBackoff(t *testing.T) {
	limited := reply(http.StatusTooManyRequests)
	p := newPlatform(t, limited, limited, limited, limited, limited)
	c, _, clk := client(nil)
	if status, _ := send(t, c, http.MethodGet, p.URL, ""); status != http.StatusTooManyRequests {
		t.Errorf("status %d after the retries", status)
	}
	if p.calls() != 4 || len(clk.sleeps) != 3 || clk.sleeps[0] != time.Second || clk.sleeps[2] != 4*time.Second {
		t.Errorf("calls %d, sleeps %v", p.calls(), clk.sleeps)
	}
}

func TestRateLimitHeaders(t *testing.T) {
	c, tr, clk := client(nil)
	reset := strconv.FormatInt(clk.Now().Add(7*time.Second).Unix(), 10)
	p := newPlatform(t, reply(http.StatusOK, "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset))
	other := newPlatform(t)
	if status, _ := send(t, c, http.MethodGet, p.URL, ""); status != http.StatusOK || len(clk.sleeps) != 0 {
		t.Fatalf("status %d, sleeps %v", status, clk.sleeps)
	}
	// Another host is not held back.
	if _, err := send(t, c, http.MethodGet, other.URL, ""); err != nil || len(clk.sleeps) != 0 {
		t.Errorf("another host waited: %v", clk.sleeps)
	}
	if _, err := send(t, c, http.MethodGet, p.URL, ""); err != nil || len(clk.sleeps) != 1 || clk.sleeps[0] != 7*time.Second {
		t.Errorf("the next call: %v, sleeps %v", err, clk.sleeps)
	}

	// Longer than MaxWait: an error now, and for the next calls, rather than a blocked request.
	p.script = []func(http.ResponseWriter){reply(http.StatusTooManyRequests, "Retry-After", "3600")}
	if _, err := send(t, c, http.MethodGet, p.URL, ""); !errors.Is(err, ErrRateLimited) {
		t.Errorf("a long Retry-After: %v", err)
	}
	if _, err := send(t, c, http.MethodGet, p.URL, ""); !errors.Is(err, ErrRateLimited) || p.calls() != 3 {
		t.Errorf("the call after: %v, %d calls", err, p.calls())
	}
	tr.MaxWait = 2 * time.Hour
	if status, err := send(t, c, http.MethodGet, p.URL, ""); err != nil || status != http.StatusOK {
		t.Errorf("with a longer MaxWait: %d, %v", status, err)
	}
}

func TestLogging(t *testing.T) {
	var logs bytes.Buffer
	c, _, _ := client(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	p := newPlatform(t, reply(http.StatusTooManyRequests, "Retry-After", "1"))
	u, _ := url.Parse(p.URL + "/_apis/git/repositories?api-version=7.0&access_token=tok-in-query")
	u.User = url.UserPassword("quai", "pass-in-url")
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	req.Header.Set("Authorization", "Basic c2VjcmV0LWhlYWRlcg==")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	out := logs.String()
	for _, secret := range []string{"tok-in-query", "pass-in-url", "c2VjcmV0LWhlYWRlcg"} {
		if strings.Contains(out, secret) {
			t.Errorf("the log shows %s:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "api-version=7.0") || !strings.Contains(out, "status=429") || !strings.Contains(out, "rate limited") {
		t.Errorf("the log misses the call:\n%s", out)
	}
}
