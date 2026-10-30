package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// Ported from the prototype's "types are read from the repository, with discovered params" and
// "brownfield files show up in the inventory with owners".
func TestCatalogAndInventory(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	bob := h.as("bob")

	var items []CatalogItem
	if status, body := bob.do(http.MethodGet, Prefix+"/catalog", nil, &items); status != http.StatusOK || len(items) != 3 {
		t.Fatalf("catalog = %d %s", status, body)
	}
	if items[0].Kind != "type" || items[0].Name != "vm" || items[1].Name != "app-servers" || items[2].Name != "linux-vm" {
		t.Errorf("catalog = %+v", items)
	}

	var vm TypeView
	if status, body := bob.do(http.MethodGet, Prefix+"/repositories/infra-live/types/vm", nil, &vm); status != http.StatusOK {
		t.Fatalf("type = %d %s", status, body)
	}
	if !slices.Equal(vm.Params[0].Options, []string{"dev", "prod", "test"}) || vm.Params[1].Pattern == "" || vm.OwnerField != "owner" ||
		vm.PropertyOrder[0] != "owner" || len(vm.Actions) != 2 || vm.FormSchema == nil || vm.Stack != "envs/{env}" || !vm.UI.Fields["template"].Immutable {
		t.Errorf("type = %+v", vm)
	}
	var bp BlueprintView
	if status, _ := bob.do(http.MethodGet, Prefix+"/repositories/infra-live/blueprints/app-servers", nil, &bp); status != http.StatusOK ||
		len(bp.Resources) != 2 || bp.Resources[0].Repository != "infra-live" || bp.Resources[1].When != "inputs.withDatabase" || len(bp.Actions) != 1 {
		t.Errorf("blueprint = %d %+v", status, bp)
	}
	if spec, _ := bp.Definition["spec"].(map[string]any); bp.Definition["kind"] != "Blueprint" || len(spec["resources"].([]any)) != 2 {
		t.Errorf("definition = %+v", bp.Definition)
	}
	for _, path := range []string{"/repositories/infra-live/types/nope", "/repositories/nope/types/vm", "/repositories/infra-live/blueprints/nope"} {
		if status, _ := bob.do(http.MethodGet, Prefix+path, nil, nil); status != http.StatusNotFound {
			t.Errorf("%s = %d", path, status)
		}
	}

	var inv []ResourceView
	if status, body := bob.do(http.MethodGet, Prefix+"/inventory", nil, &inv); status != http.StatusOK || len(inv) != 4 {
		t.Fatalf("inventory = %d %s", status, body)
	}
	owners := map[string]string{}
	for _, r := range inv {
		owners[r.ID] = r.Owner
		if r.Status != "conforming" || r.Type != "vm" {
			t.Errorf("resource %+v", r)
		}
	}
	if owners["infra-live/vm/prod/web01"] != "bob" || owners["infra-live/vm/prod/db01"] != "dana" || inv[2].Stack != "envs/prod" {
		t.Errorf("owners = %v, %+v", owners, inv[2])
	}
	if status, _ := bob.do(http.MethodGet, Prefix+"/inventory?type=nope", nil, &inv); status != http.StatusOK || len(inv) != 0 {
		t.Errorf("inventory of no type = %d %d", status, len(inv))
	}

	var d ResourceDetail
	if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/vm/prod/web01", nil, &d); status != http.StatusOK {
		t.Fatalf("resource = %d %s", status, body)
	}
	if d.Document["cpu"] != 4.0 || !strings.Contains(d.Raw, "# bumped in INC-4411") || len(d.History) != 1 || d.History[0].Subject != "the existing infrastructure" || len(d.Actions) != 2 {
		t.Errorf("resource = %+v", d)
	}
	for _, id := range []string{"infra-live/vm/prod/nope", "bad", "infra-live/nope/x", "nope/vm/prod/web01"} {
		if status, _ := bob.do(http.MethodGet, Prefix+"/resource?id="+id, nil, nil); status != http.StatusNotFound {
			t.Errorf("resource %s = %d", id, status)
		}
	}
}

func TestSyncAndProblems(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	alice := h.as("alice")
	h.push("envs/test/vms/t1.yaml", "owner: carol\ntemplate: rhel-9\ncpu: 2\nmemory_gb: 4\ndisks: [{ size_gb: 20 }]\nnetwork: { vlan: 110 }\n")
	h.push("envs/test/vms/broken.yaml", "cpu: [2\n")
	h.push("envs/test/vms/wrong.yaml", "cpu: 99\n")
	h.push(".quai/types/bad.yaml", "apiVersion: quai.dev/v1alpha1\nkind: ResourceType\n")
	var repo RepositoryView
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories/infra-live/sync", nil, &repo); status != http.StatusOK || len(repo.Problems) != 1 || repo.Types != 1 {
		t.Fatalf("sync = %d %s", status, body)
	}
	var inv []ResourceView
	alice.do(http.MethodGet, Prefix+"/inventory?repository=infra-live", nil, &inv)
	status := map[string]string{}
	for _, r := range inv {
		status[r.Key] = r.Status
	}
	if status["test/t1"] != "conforming" || status["test/broken"] != "unparseable" || status["test/wrong"] != "non-conforming" {
		t.Errorf("statuses = %v", status)
	}
	var repos []RepositoryView
	if code, _ := alice.do(http.MethodGet, Prefix+"/repositories", nil, &repos); code != http.StatusOK || len(repos) != 1 || repos[0].Commit != repo.Commit ||
		repos[0].Connector != "generic" || repos[0].ManagedBy != "config" {
		t.Errorf("repositories = %d %+v", code, repos)
	}
	if code, _ := alice.do(http.MethodPost, Prefix+"/repositories/nope/sync", nil, nil); code != http.StatusNotFound {
		t.Errorf("sync of nothing = %d", code)
	}
}

func TestAnonymousCatalog(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	nobody := h.as("")
	for _, path := range []string{"/catalog", "/inventory", "/repositories", "/repositories/infra-live/types/vm", "/resource?id=infra-live/vm/prod/web01"} {
		if status, _ := nobody.do(http.MethodGet, Prefix+path, nil, nil); status != http.StatusUnauthorized {
			t.Errorf("anonymous %s = %d", path, status)
		}
	}
	// fiona (finops) is a viewer: she sees the inventory but cannot request.
	fiona := h.as("fiona")
	var inv []ResourceView
	if status, _ := fiona.do(http.MethodGet, Prefix+"/inventory", nil, &inv); status != http.StatusOK || len(inv) != 4 {
		t.Errorf("fiona's inventory = %d %d", status, len(inv))
	}
}
