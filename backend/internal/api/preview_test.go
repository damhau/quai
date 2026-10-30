package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// branch commits content to file on a new branch of the remote, and goes back to main.
func (h *harness) branch(name, file, content string) {
	h.t.Helper()
	git(h.t, h.work, "checkout", "--quiet", "-b", name)
	if err := os.WriteFile(filepath.Join(h.work, filepath.FromSlash(file)), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	git(h.t, h.work, "commit", "--quiet", "--all", "-m", "edit "+file)
	git(h.t, h.work, "push", "--quiet", "origin", name)
	git(h.t, h.work, "checkout", "--quiet", "main")
}

// TestPreview: catalog authors see a branch's definitions before they are merged; the catalog
// keeps the default branch's, and nothing is requested from a preview.
func TestPreview(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	alice, bob := h.as("alice"), h.as("bob")
	original, _ := h.file(".quai/types/vm.yaml")
	h.branch("vm-v2", ".quai/types/vm.yaml", strings.Replace(original, "Virtual machine (Terraform)", "Virtual machine v2", 1))
	h.branch("vm-broken", ".quai/types/vm.yaml", strings.Replace(original, "kind: ResourceType", "kind: ResourceTyp", 1))

	var p PreviewView
	if status, body := alice.do(http.MethodGet, Prefix+"/repositories/infra-live/preview?branch=vm-v2", nil, &p); status != http.StatusOK {
		t.Fatalf("preview = %d %s", status, body)
	}
	if p.Branch != "vm-v2" || p.Commit == "" || len(p.Problems) != 0 || len(p.Items) == 0 || p.Items[0].Name != "vm" || p.Items[0].Title != "Virtual machine v2" {
		t.Errorf("preview = %+v", p)
	}
	var vt TypeView
	if status, body := alice.do(http.MethodGet, Prefix+"/repositories/infra-live/preview/types/vm?branch=vm-v2", nil, &vt); status != http.StatusOK || vt.Title != "Virtual machine v2" || len(vt.Params) == 0 {
		t.Errorf("preview type = %d %s", status, body)
	}
	var bv BlueprintView
	if status, body := alice.do(http.MethodGet, Prefix+"/repositories/infra-live/preview/blueprints/linux-vm?branch=vm-v2", nil, &bv); status != http.StatusOK || bv.Definition == nil {
		t.Errorf("preview blueprint = %d %s", status, body)
	}
	// The catalog keeps the default branch's definitions.
	if status, _ := alice.do(http.MethodGet, Prefix+"/repositories/infra-live/types/vm", nil, &vt); status != http.StatusOK || vt.Title != "Virtual machine (Terraform)" {
		t.Errorf("the catalog shows %q", vt.Title)
	}
	// A broken branch shows its problems.
	if status, _ := alice.do(http.MethodGet, Prefix+"/repositories/infra-live/preview?branch=vm-broken", nil, &p); status != http.StatusOK || len(p.Problems) == 0 || p.Problems[0].Path != ".quai/types/vm.yaml" {
		t.Errorf("a broken branch = %+v", p)
	}

	for name, tc := range map[string]struct {
		u    *user
		path string
		want int
	}{
		"not a catalog author":          {bob, "/preview?branch=vm-v2", http.StatusForbidden},
		"no ref":                        {alice, "/preview", http.StatusBadRequest},
		"a missing branch":              {alice, "/preview?branch=nope", http.StatusNotFound},
		"a pull request on generic Git": {alice, "/preview?pullRequest=1", http.StatusUnprocessableEntity},
		"a missing type":                {alice, "/preview/types/nope?branch=vm-v2", http.StatusNotFound},
	} {
		if status, body := tc.u.do(http.MethodGet, Prefix+"/repositories/infra-live"+tc.path, nil, nil); status != tc.want {
			t.Errorf("%s = %d %s", name, status, body)
		}
	}
}
