package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runLint(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(append([]string{"lint"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestLintExamples(t *testing.T) {
	out, err := runLint(t, "../../../examples/gitops-repo")
	if err != nil || !strings.Contains(out, "1 types, 2 blueprints, 4 resources checked: no problems") {
		t.Errorf("lint = %q, %v", out, err)
	}
}

func TestLintProblems(t *testing.T) {
	dir := t.TempDir()
	vms := filepath.Join(dir, "envs", "dev", "vms")
	if err := os.MkdirAll(vms, 0o750); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("../../../examples/gitops-repo/.quai/types/vm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for p, b := range map[string][]byte{
		".quai/types/vm.yaml":            src,
		"catalog/schemas/vm.schema.json": []byte(`{"type": "object", "required": ["cpu"]}`),
		"envs/dev/vms/a.yaml":            []byte("memory_gb: 4\n"),
	} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runLint(t, dir)
	if err == nil || err.Error() != "1 problem in "+dir {
		t.Errorf("error = %v", err)
	}
	if !strings.Contains(out, "envs/dev/vms/a.yaml: cpu: is required (schema catalog/schemas/vm.schema.json)") || !strings.Contains(out, "1 resources checked: 1 problem") {
		t.Errorf("output = %q", out)
	}
	if _, err := runLint(t, filepath.Join(dir, "nope")); err == nil {
		t.Error("lint of a missing directory succeeded")
	}
	if _, err := runLint(t, filepath.Join(dir, ".quai", "types", "vm.yaml")); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("lint of a file: %v", err)
	}
	if plural(2, "problem") != "2 problems" {
		t.Error("plural")
	}
}

func TestLintPlan(t *testing.T) {
	plans := "../../internal/engine/tfplan/testdata/"
	if out, err := runLint(t, "../../../examples/gitops-repo", "--plan", plans+"prod.json", "--stack", "envs/prod"); err != nil {
		t.Errorf("the example stack's plan = %q, %v", out, err)
	}
	out, err := runLint(t, "../../../examples/gitops-repo", "--plan", plans+"nested.json", "--stack", "envs/prod")
	if err == nil || !strings.Contains(out, `terraform_data.unclaimed: created by no resource`) || !strings.Contains(out, "3 problems") {
		t.Errorf("unclaimed resources = %q, %v", out, err)
	}
	if _, err := runLint(t, "../../../examples/gitops-repo", "--plan", plans+"prod.json"); err == nil || !strings.Contains(err.Error(), "go together") {
		t.Errorf("--plan without --stack = %v", err)
	}
}
