package connectors

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// NewHTTPClient returns the client connectors make their API calls with: over base (netx's
// transport, with the CA bundle and the proxy), honouring the platforms' rate limits.
func NewHTTPClient(base http.RoundTripper, log *slog.Logger) *http.Client {
	return &http.Client{Transport: NewTransport(base, log), Timeout: Timeout}
}

// Transport honours rate limits and logs calls without their secrets.
//
//   - A response with Retry-After (429, 503, or a delayed success on Azure DevOps) holds back
//     the next calls to that host until then.
//   - X-RateLimit-Remaining: 0 holds them back until X-RateLimit-Reset.
//   - A 429 is retried after the wait, or with exponential backoff without Retry-After; a 503
//     with Retry-After is retried when the method is idempotent.
//   - A wait longer than MaxWait fails with ErrRateLimited rather than blocking.
//
// Calls are logged at debug level with their method, URL (credentials and secret-looking query
// values removed), status and duration; headers and bodies never are.
type Transport struct {
	base    http.RoundTripper
	log     *slog.Logger
	MaxWait time.Duration // the longest wait for a rate limit
	Retries int           // the retries of a rate-limited call
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error

	mu        sync.Mutex
	notBefore map[string]time.Time // by host
}

// NewTransport returns a Transport over base.
func NewTransport(base http.RoundTripper, log *slog.Logger) *Transport {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Transport{base: base, log: log, MaxWait: time.Minute, Retries: 3, now: time.Now, sleep: sleep, notBefore: map[string]time.Time{}}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, host := req.Context(), req.URL.Host
	for attempt := 0; ; attempt++ {
		if err := t.wait(ctx, host); err != nil {
			return nil, err
		}
		r := req
		if attempt > 0 {
			r = req.Clone(ctx)
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				r.Body = body
			}
		}
		start := t.now()
		resp, err := t.base.RoundTrip(r)
		if err != nil {
			t.log.DebugContext(ctx, "API call failed", slog.String("method", req.Method), slog.String("url", Redact(req.URL)), slog.Any("error", err))
			return nil, err
		}
		delay := t.observe(host, resp)
		t.log.DebugContext(ctx, "API call", slog.String("method", req.Method), slog.String("url", Redact(req.URL)),
			slog.Int("status", resp.StatusCode), slog.Duration("duration", t.now().Sub(start)), slog.Int("attempt", attempt+1))
		retry := resp.StatusCode == http.StatusTooManyRequests ||
			(resp.StatusCode == http.StatusServiceUnavailable && delay > 0 && idempotent(req.Method))
		rewindable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
		if !retry || attempt >= t.Retries || !rewindable {
			return resp, nil
		}
		if delay == 0 {
			delay = time.Second << attempt // backoff: 1s, 2s, 4s
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		if delay > t.MaxWait {
			return nil, fmt.Errorf("%w: %s asks to wait %s", ErrRateLimited, host, delay)
		}
		t.log.WarnContext(ctx, "rate limited by the platform: waiting", slog.String("host", host), slog.Duration("wait", delay), slog.Int("status", resp.StatusCode))
		t.hold(host, t.now().Add(delay))
	}
}

// wait holds a call back until the host's rate limit allows it.
func (t *Transport) wait(ctx context.Context, host string) error {
	t.mu.Lock()
	until := t.notBefore[host]
	t.mu.Unlock()
	d := until.Sub(t.now())
	if d <= 0 {
		return nil
	}
	if d > t.MaxWait {
		return fmt.Errorf("%w: %s asks to wait until %s", ErrRateLimited, host, until.UTC().Format(time.RFC3339))
	}
	return t.sleep(ctx, d)
}

// hold keeps calls to host from going out before until.
func (t *Transport) hold(host string, until time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if until.After(t.notBefore[host]) {
		t.notBefore[host] = until
	}
}

// observe records the rate limits a response announces, and returns its Retry-After.
func (t *Transport) observe(host string, resp *http.Response) time.Duration {
	now := t.now()
	var delay time.Duration
	if v := resp.Header.Get("Retry-After"); v != "" {
		if s, err := strconv.Atoi(v); err == nil && s >= 0 {
			delay = time.Duration(s) * time.Second
		} else if at, err := http.ParseTime(v); err == nil {
			delay = max(at.Sub(now), 0)
		}
		t.hold(host, now.Add(delay))
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			t.hold(host, time.Unix(reset, 0))
		}
	}
	return delay
}

func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// secretParam matches the query parameters whose values are not logged.
var secretParam = regexp.MustCompile(`(?i)token|key|secret|password|passwd|sig|code|auth`)

// Redact returns u without its credentials, and with the values of secret-looking query
// parameters replaced by ***.
func Redact(u *url.URL) string {
	c := *u
	if c.User != nil {
		c.User = url.User("***")
	}
	if c.RawQuery != "" {
		q := c.Query()
		for k := range q {
			if secretParam.MatchString(k) {
				q[k] = []string{"***"}
			}
		}
		c.RawQuery = q.Encode()
	}
	return c.String()
}
