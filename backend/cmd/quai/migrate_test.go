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

	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"

	"github.com/damhau/quai/backend/internal/audit/keeper"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/store"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// TestMigrate: migrations run as the schema's owner, who grants the application role the audit
// log to read and append; with one role, a warning says the audit log is not protected.
func TestMigrate(t *testing.T) {
	url := pgtest.URL(t)
	migrate := func(database string) (string, string) {
		t.Helper()
		conf := filepath.Join(t.TempDir(), "quai.yaml")
		if err := os.WriteFile(conf, []byte("database: "+database+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := newRootCmd()
		var out, warn bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&warn)
		cmd.SetArgs([]string{"migrate", "--config", conf})
		if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), "0 migrations applied") {
			t.Errorf("migrate on a migrated database = %q, %v", out.String(), err)
		}
		return out.String(), warn.String()
	}
	out, warn := migrate("{ url: \"" + url + "\", migrateURL: \"" + pgtest.OwnerURL(url) + "\" }\naudit: { retention: 87600h }")
	if !strings.Contains(out, pgtest.AppRole+" may read and append the audit log, and not change it") || !strings.Contains(out, "audit events are kept 87600h0m0s") || warn != "" {
		t.Errorf("two roles: %q, %q", out, warn)
	}
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if r, err := s.AuditRetention(context.Background()); err != nil || r.Microseconds != (87600*time.Hour).Microseconds() || r.Days != 0 || r.Months != 0 {
		t.Errorf("retention = %+v, %v", r, err)
	}
	if _, warn = migrate("{ url: \"" + url + "\" }\naudit: { retention: 87600h }"); !strings.Contains(warn, "the audit log is not protected") {
		t.Errorf("one role: %q", warn)
	}
	empty := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(empty, []byte("env: development\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCmd()
	cmd.SetArgs([]string{"migrate", "--config", empty})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Errorf("migrate without a database: %v", err)
	}
}

// auditKey sets a checkpoint signing key in the environment, as QUAI_TEST_AUDIT_KEY.
func auditKey(t *testing.T) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUAI_TEST_AUDIT_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
}

// TestWorker: the worker runs the jobs, among them the audit log's checkpoint and verification
// at start, and signs a last checkpoint as it stops.
func TestWorker(t *testing.T) {
	s := pgtest.Store(t)
	auditKey(t)
	cfg, err := config.Load("", func(n string) (string, bool) {
		return map[string]string{"QUAI_AUDIT_CHECKPOINTS_SIGNING_KEY_REF": "env:QUAI_TEST_AUDIT_KEY"}[n], n == "QUAI_AUDIT_CHECKPOINTS_SIGNING_KEY_REF"
	})
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
		if _, err := s.LastAuditVerification(ctx); err != nil {
			started = false // the verification at start is not recorded yet
		}
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
	// The verification at start is recorded, and the checkpoint at shutdown covers it.
	v, err := s.LastAuditVerification(context.Background())
	if err != nil || v.Action != keeper.ActionVerified {
		t.Errorf("verification = %+v, %v", v, err)
	}
	if c, err := s.LatestAuditCheckpoint(context.Background()); err != nil || c.Seq != v.Seq {
		t.Errorf("checkpoint at shutdown = %+v, %v; the verification is seq %d", c, err, v.Seq)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"worker"})
	cmd.SetErr(io.Discard)
	t.Setenv("QUAI_DATABASE_URL", "")
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Errorf("worker without a database: %v", err)
	}
}
