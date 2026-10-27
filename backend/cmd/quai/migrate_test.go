package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
