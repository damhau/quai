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
	"github.com/damhau/quai/backend/internal/evidence"
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

// TestAuditVerifyBundle: quai audit verify --bundle checks a request's bundle offline with the
// public key: it passes on one Quai made, fails on an edited one, and on unproven events unless
// --allow-unproven.
func TestAuditVerifyBundle(t *testing.T) {
	ctx := context.Background()
	s := pgtest.Store(t)
	dir := t.TempDir()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, _ := audit.MarshalPublicKey(pub)
	keyFile := writeFile(t, dir, "audit.pub", string(pubPEM))
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	otherPEM, _ := audit.MarshalPublicKey(otherKey.Public().(ed25519.PublicKey))
	otherFile := writeFile(t, dir, "other.pub", string(otherPEM))
	id := "0b9d5a52-6c1e-4f43-9d0e-6f1f1c1a2b3c"
	record := func(action string, details map[string]any) {
		t.Helper()
		if err := s.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			return audit.Add(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.User, ID: "Bob Builder", IP: "10.0.0.2"}, Action: action,
				Object: audit.Object{Type: "request", ID: id}, Details: details})
		}); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name string) (string, *evidence.Bundle) {
		t.Helper()
		a, err := evidence.Prove(ctx, s.Queries, id, "")
		if err != nil {
			t.Fatal(err)
		}
		b := &evidence.Bundle{BundleVersion: evidence.Version, GeneratedAt: "2026-11-02T12:00:00.000Z", Quai: "test",
			Request: evidence.Request{ID: id, Number: 7, Kind: "create", Repository: "infra-live", Requester: "Bob Builder", Reason: "A new VM.",
				State: "pending_approval", Inputs: json.RawMessage(`{}`), Owners: []string{}, Rules: []evidence.Rule{}},
			ChangeSets: []evidence.ChangeSet{}, Plans: []evidence.Run{}, Approvals: []evidence.Approval{}, Applies: []evidence.Run{},
			Events: a.Events, Personal: a.Personal, Proof: a.Proof, Notes: []string{}}
		data, err := evidence.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		return writeFile(t, dir, name, string(data)), b
	}
	record("request.submitted", map[string]any{"kind": "create", "repository": "infra-live", "reason": audit.Personal("A new VM.")})
	if _, _, err := audit.SignLatest(ctx, s.Queries, key, time.Now()); err != nil {
		t.Fatal(err)
	}
	proven, b := write("proven.json")

	if out, _, err := runAudit(t, "", "verify", "--bundle", proven, "--key", keyFile); err != nil ||
		out != "request #7 "+id+": 1 event, 1 proven by 1 checkpoint (seq 1) with no records of other events\nthe bundle verifies\n" {
		t.Errorf("verify --bundle = %q, %v", out, err)
	}
	data, _ := os.ReadFile(proven)
	if out, _, err := runAudit(t, string(data), "verify", "--bundle", "-", "--key", keyFile); err != nil || !strings.HasSuffix(out, "the bundle verifies\n") {
		t.Errorf("verify --bundle - = %q, %v", out, err)
	}
	if _, _, err := runAudit(t, "", "verify", "--bundle", proven, "--key", otherFile); err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Errorf("verified with another key: %v", err)
	}
	if _, _, err := runAudit(t, "", "verify", "--bundle", proven); err == nil || !strings.Contains(err.Error(), "--key is needed") {
		t.Errorf("verified without a key: %v", err)
	}

	// An edited bundle: the reason in its summary.
	b.Request.Reason = "Nothing to see."
	edited, _ := evidence.Marshal(b)
	if _, _, err := runAudit(t, string(edited), "verify", "--bundle", "-", "--key", keyFile); err == nil || !strings.Contains(err.Error(), "the summary's reason") {
		t.Errorf("an edited bundle: %v", err)
	}
	if _, _, err := runAudit(t, `{"bundleVersion":2}`, "verify", "--bundle", "-", "--key", keyFile); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("a bundle of another version: %v", err)
	}

	// An event after the latest checkpoint is unproven: an error unless accepted.
	record("request.reminded", map[string]any{})
	unproven, _ := write("unproven.json")
	if out, _, err := runAudit(t, "", "verify", "--bundle", unproven, "--key", keyFile); err == nil || !strings.Contains(err.Error(), "1 event of the request unproven") ||
		!strings.Contains(out, "unproven: seq 2: no signed checkpoint follows it yet") {
		t.Errorf("an unproven event = %q, %v", out, err)
	}
	if out, _, err := runAudit(t, "", "verify", "--bundle", unproven, "--key", keyFile, "--allow-unproven"); err != nil ||
		!strings.HasSuffix(out, "the bundle verifies, with 1 event of the request unproven\n") {
		t.Errorf("--allow-unproven = %q, %v", out, err)
	}
}
