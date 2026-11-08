package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/audit/keeper"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/store/db"
)

func runAudit(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out, warn bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&warn)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append([]string{"audit"}, args...))
	err := cmd.Execute()
	return out.String(), warn.String(), err
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAuditExportVerify: quai audit export writes the log, and quai audit verify checks it offline
// with the public key, and the audit repository's checkpoints, naming where it breaks.
func TestAuditExportVerify(t *testing.T) {
	ctx := context.Background()
	s := pgtest.Store(t)
	dir := t.TempDir()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, _ := audit.MarshalPublicKey(pub)
	keyFile := writeFile(t, dir, "audit.pub", string(pubPEM))
	record := func(n int) {
		t.Helper()
		for i := range n {
			if err := s.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
				return audit.Add(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.User, ID: "alice"}, Action: "auth.login",
					Object: audit.Object{Type: "user", ID: "alice"}, Details: map[string]any{"n": i}})
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	sign := func() audit.Checkpoint {
		t.Helper()
		c, _, err := audit.SignLatest(ctx, s.Queries, key, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	record(3)
	c3 := sign()
	record(2)
	conf := writeFile(t, dir, "quai.yaml", "database: { url: \""+s.Pool.Config().ConnString()+"\" }\n")

	// Export, to a file and to standard output.
	exported := filepath.Join(dir, "audit.jsonl")
	if out, warn, err := runAudit(t, "", "export", "--config", conf, "--output", exported); err != nil || out != "" || warn != "5 audit events exported\n" {
		t.Fatalf("export = %q, %q, %v", out, warn, err)
	}
	b, err := os.ReadFile(exported)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(exported); info.Mode().Perm() != 0o600 {
		t.Errorf("export mode %v", info.Mode())
	}
	if out, _, err := runAudit(t, "", "export", "--config", conf); err != nil || out != string(b) {
		t.Errorf("export to standard output = %q, %v; to a file %q", out, err, b)
	}
	if _, _, err := runAudit(t, "", "export", "--config", conf, "-o", exported); err == nil {
		t.Error("export over an existing file")
	}
	if strings.Contains(string(b), "alice") {
		t.Error("the export holds a clear personal value")
	}

	// Verify.
	if out, _, err := runAudit(t, "", "verify", "--key", keyFile, exported); err != nil || out != "5 events verified from seq 1 to 5, 1 checkpoint matching their events\n" {
		t.Errorf("verify = %q, %v", out, err)
	}
	if out, _, err := runAudit(t, string(b), "verify", "--key", keyFile, "-"); err != nil || !strings.HasPrefix(out, "5 events verified") {
		t.Errorf("verify standard input = %q, %v", out, err)
	}
	if _, _, err := runAudit(t, "", "verify", exported); err == nil || err.Error() != "the audit log breaks at seq 3: the checkpoint does not hold: it is signed with key "+c3.Key+", which was not given" {
		t.Errorf("verify without the key: %v", err)
	}
	tampered := writeFile(t, dir, "tampered.jsonl", strings.Replace(string(b), `"n":1`, `"n":7`, 1))
	if _, _, err := runAudit(t, "", "verify", "--key", keyFile, tampered); err == nil || err.Error() != "the audit log breaks at seq 2: its hash is not the hash of its content" {
		t.Errorf("verify a changed event: %v", err)
	}

	// With the audit repository's checkpoints: one of the export, then one beyond it.
	repo := t.TempDir()
	for _, c := range []audit.Checkpoint{c3} {
		file, _ := keeper.File(c)
		j, _ := json.MarshalIndent(c, "", "  ")
		writeFile(t, repo, file, string(j))
	}
	writeFile(t, repo, ".git/config", "[core]\n")
	writeFile(t, repo, "README.md", "audit\n")
	if out, _, err := runAudit(t, "", "verify", "--key", keyFile, "--anchors", repo, exported); err != nil || out != "5 events verified from seq 1 to 5, 1 checkpoint matching their events, with 1 checkpoint from the audit repository\n" {
		t.Errorf("verify with anchors = %q, %v", out, err)
	}
	record(1)
	c6 := sign()
	file, _ := keeper.File(c6)
	j, _ := json.Marshal(c6)
	writeFile(t, repo, file, string(j))
	if _, _, err := runAudit(t, "", "verify", "--key", keyFile, "--anchors", repo, exported); err == nil || !strings.HasPrefix(err.Error(), "the audit log breaks at seq 6: the event is missing: the log ends at seq 5, but a checkpoint in the audit repository says it reached seq 6") {
		t.Errorf("verify with an anchor beyond the export: %v", err)
	}

	// What it refuses.
	for name, args := range map[string][]string{
		"a key that is not one":    {"verify", "--key", exported, exported},
		"an anchor that is not":    {"verify", "--key", keyFile, "--anchors", exported, exported},
		"an export that is not":    {"verify", "--key", keyFile, keyFile},
		"an empty export":          {"verify", "--key", keyFile, writeFile(t, dir, "empty.jsonl", "")},
		"a missing export":         {"verify", filepath.Join(dir, "nope")},
		"an export with no config": {"export", "--config", writeFile(t, dir, "empty.yaml", "env: development\n")},
	} {
		if _, _, err := runAudit(t, "", args...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
