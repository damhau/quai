package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func TestMigrate(t *testing.T) {
	url := pgtest.URL(t)
	conf := filepath.Join(t.TempDir(), "quai.yaml")
	if err := os.WriteFile(conf, []byte("database: { url: \""+url+"\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"migrate", "--config", conf})
	if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), "0 migrations applied") {
		t.Errorf("migrate on a migrated database = %q, %v", out.String(), err)
	}
	empty := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(empty, []byte("env: development\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = newRootCmd()
	cmd.SetArgs([]string{"migrate", "--config", empty})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Errorf("migrate without a database: %v", err)
	}
}

func TestWorker(t *testing.T) {
	s := pgtest.Store(t)
	cfg, err := config.Load("", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	sw := &syncWriter{w: &logs}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- work(ctx, cfg, slog.New(slog.NewTextHandler(sw, nil)), s, nil) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		sw.mu.Lock()
		started := strings.Contains(logs.String(), "msg=working")
		sw.mu.Unlock()
		if started || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if !strings.Contains(logs.String(), "msg=working") || !strings.Contains(logs.String(), "msg=stopped") {
		t.Errorf("logs = %q", logs.String())
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"worker"})
	cmd.SetErr(io.Discard)
	t.Setenv("QUAI_DATABASE_URL", "")
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Errorf("worker without a database: %v", err)
	}
}
