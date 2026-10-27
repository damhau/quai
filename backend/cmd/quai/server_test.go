package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/pgtest"
)

func TestServe(t *testing.T) {
	cfg, err := config.Load("", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	sw := &syncWriter{w: &logs}
	go func() { done <- serve(ctx, cfg, slog.New(slog.NewTextHandler(sw, nil)), ln, pgtest.Store(t)) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Errorf("/healthz = %d %q", resp.StatusCode, body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if !strings.Contains(logs.String(), "msg=listening") || !strings.Contains(logs.String(), "msg=stopped") {
		t.Errorf("logs = %q", logs.String())
	}

	// A listener that fails ends serve with an error.
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	_ = ln2.Close()
	if err := serve(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), ln2, nil); err == nil {
		t.Error("serve on a closed listener succeeded")
	}
}

func TestServerCommand(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"server", "--config", "/nonexistent/quai.yaml"})
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "configuration") {
		t.Errorf("server with a missing file: %v", err)
	}
}

// syncWriter lets the server's goroutines and the test share a buffer.
type syncWriter struct {
	w  io.Writer
	mu sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
