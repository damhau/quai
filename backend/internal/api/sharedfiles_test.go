package api

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/adotest"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/azuredevops"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// exampleFile reads a file of examples/gitops-repo.
func exampleFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile("../../../examples/gitops-repo/" + p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// changed returns the lines a change adds or removes, as diffText writes them.
func changed(c ChangeView) string {
	var b strings.Builder
	for _, l := range c.Diff {
		if l.Op == "+" || l.Op == "-" {
			b.WriteString(l.Op + l.Text + "\n")
		}
	}
	return b.String()
}

// sharedFilesFlow is the shared-file flow of examples/gitops-repo (P3-20) on h, whose repository
// is the example delivered by pull requests that Quai merges once approved; read returns a file
// of the remote's main. A namespace is created and edited in place; carol, who does not own it,
// has its quota grow through the namespace-quota field type; the shop's database record moves
// beside a TTL defined in code; two record sets of one zone are approved together and both merge;
// a database of the mapping type is created and resized. Each change writes its entry's bytes
// only. The end-to-end scenarios (e2e/tests/shared-files.spec.ts) walk the same flow in the UI.
func sharedFilesFlow(t *testing.T, h *harness, read func(path string) string) {
	t.Helper()
	bob, carol, alice := h.as("bob"), h.as("carol"), h.as("alice")
	namespaces := "clusters/dev-1/namespaces.yaml"

	// A new namespace on dev-1: the platform admins approve it.
	ns := bob.request(map[string]any{"type": "namespace", "kind": "create", "params": map[string]string{"cluster": "dev-1", "name": "search-dev"},
		"values": map[string]any{"owner": "bob", "quota": map[string]any{"cpu": 2, "memory_gi": 4}}})
	if names := ruleNames(ns); !slices.Equal(names, []string{"type/platform"}) {
		t.Errorf("the rules of a new namespace: %v", names)
	}
	alice.approve(ns.ID)
	bob.awaitMerged(ns.ID)
	entry := "  - name: search-dev\n    owner: bob\n    quota:\n      cpu: 2\n      memory_gi: 4\n"
	if got, want := read(namespaces), exampleFile(t, namespaces)+entry; got != want {
		t.Errorf("%s after the new namespace:\n%s", namespaces, got)
	}

	// Its owner describes it: on dev-1, with the same quota, no one needs to approve. The new key
	// ends the entry; nothing else changes.
	described := bob.request(map[string]any{"type": "namespace", "kind": "update", "key": "dev-1/search-dev",
		"values": map[string]any{"owner": "bob", "description": "Search API", "quota": map[string]any{"cpu": 2, "memory_gi": 4}}})
	if len(described.Rules) != 0 || changed(described.Changes[0]) != "+    description: Search API\n" {
		t.Errorf("the description: rules %v, diff:\n%s", ruleNames(described), diffText(described.Changes[0]))
	}
	bob.awaitMerged(described.ID)
	entry += "    description: Search API\n"

	// More quota, asked by carol through the field type: only the quota changes, and it grows,
	// so the platform admins approve it.
	quota := carol.request(map[string]any{"type": "namespace-quota", "kind": "update", "key": "dev-1/search-dev",
		"values": map[string]any{"quota": map[string]any{"cpu": 6, "memory_gi": 4}}})
	if names := ruleNames(quota); !slices.Equal(names, []string{"type/platform"}) || changed(quota.Changes[0]) != "-      cpu: 2\n+      cpu: 6\n" {
		t.Errorf("the quota: rules %v, diff:\n%s", names, diffText(quota.Changes[0]))
	}
	alice.approve(quota.ID)
	carol.awaitMerged(quota.ID)
	entry = strings.Replace(entry, "cpu: 2", "cpu: 6", 1)
	if got, want := read(namespaces), exampleFile(t, namespaces)+entry; got != want {
		t.Errorf("%s after the quota:\n%s", namespaces, got)
	}

	// The shop's database moves: its TTL is an expression, shown as such and kept as it is.
	records := "dns/shop.internal/records.auto.tfvars"
	var db ResourceDetail
	if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/dns-record/shop.internal/shop-db", nil, &db); status != http.StatusOK ||
		len(db.DefinedInCode) != 1 || db.DefinedInCode[0].Field != "ttl" || db.DefinedInCode[0].Text != "24 * 3600" {
		t.Fatalf("shop-db = %d %s", status, body)
	}
	moved := bob.request(map[string]any{"type": "dns-record", "kind": "update", "key": "shop.internal/shop-db",
		"values": map[string]any{"owner": "bob", "type": "A", "values": []string{"10.0.13.31"}}})
	if changed(moved.Changes[0]) != "-    values = [\"10.0.13.30\"]\n+    values = [\"10.0.13.31\"]\n" {
		t.Errorf("the move:\n%s", diffText(moved.Changes[0]))
	}
	alice.approve(moved.ID)
	bob.awaitMerged(moved.ID)
	// The golden example of the move says the same (.quai/tests/dns-record/move-shop-db).
	zone := exampleFile(t, ".quai/tests/dns-record/move-shop-db/after/"+records)
	if got := read(records); got != zone {
		t.Errorf("%s after the move:\n%s", records, got)
	}

	// Two record sets of the zone, both approved before either merges: both append to the map, so
	// the second to merge is rebuilt on the first and keeps its approval.
	a := bob.request(map[string]any{"type": "dns-record", "kind": "create", "params": map[string]string{"zone": "shop.internal", "name": "api"},
		"values": map[string]any{"owner": "bob", "type": "A", "values": []string{"10.0.12.40"}}})
	b := bob.request(map[string]any{"type": "dns-record", "kind": "create", "params": map[string]string{"zone": "shop.internal", "name": "cdn"},
		"values": map[string]any{"owner": "bob", "type": "CNAME", "values": []string{"web01.shop.internal."}}})
	alice.approve(a.ID)
	alice.approve(b.ID)
	a, b = bob.awaitMerged(a.ID), bob.awaitMerged(b.ID)
	second := b
	if hasEvent(a, "rebuilt", "") {
		second = a
	}
	if !hasEvent(second, "rebuilt", "") || len(second.Reviews) != 1 {
		t.Errorf("the second record set to merge: %+v, reviews %+v", second.Events, second.Reviews)
	}
	api := "  api = {\n    owner  = \"bob\"\n    type   = \"A\"\n    values = [\"10.0.12.40\"]\n  }\n"
	cdn := "  cdn = {\n    owner  = \"bob\"\n    type   = \"CNAME\"\n    values = [\"web01.shop.internal.\"]\n  }\n"
	head := strings.TrimSuffix(zone, "}\n")
	if got := read(records); got != head+api+cdn+"}\n" && got != head+cdn+api+"}\n" {
		t.Errorf("%s after both record sets:\n%s", records, got)
	}

	// A database: the form asks for a size, the template writes the resources; resized, only
	// they change. In dev, no one needs to approve either.
	databases := "databases/dev/databases.auto.tfvars.json"
	created := bob.request(map[string]any{"type": "database", "kind": "create", "params": map[string]string{"env": "dev", "name": "orders-dev"},
		"values": map[string]any{"engine": "postgres", "size": "small"}})
	if len(created.Rules) != 0 {
		t.Errorf("the rules of a small database in dev: %v", ruleNames(created))
	}
	bob.awaitMerged(created.ID)
	resized := bob.request(map[string]any{"type": "database", "kind": "update", "key": "dev/orders-dev",
		"values": map[string]any{"engine": "postgres", "size": "medium"}})
	bob.awaitMerged(resized.ID)
	// The golden example of a medium database in dev says what the file holds then
	// (.quai/tests/database/create-dev-orders), the owner aside.
	want := strings.Replace(exampleFile(t, ".quai/tests/database/create-dev-orders/after/"+databases), `"owner": "carol"`, `"owner": "bob"`, 1)
	if got := read(databases); got != want {
		t.Errorf("%s after the resize:\n%s\ndiff of the resize:\n%s", databases, got, diffText(resized.Changes[0]))
	}
}

// TestSharedFiles: the shared-file flow of the example repository, on Gitea.
func TestSharedFiles(t *testing.T) {
	t.Parallel()
	h, remote := giteaHarness(t, "auto")
	sharedFilesFlow(t, h, func(p string) string { return mainFile(t, remote, p) })
}

// TestAzureDevOpsSharedFiles: the shared-file flow of the example repository on the Azure DevOps
// sandbox, in a throwaway repository whose main requires quai/approval; Azure DevOps completes
// the pull requests Quai sets to auto-complete. Skipped without the sandbox's PAT.
func TestAzureDevOpsSharedFiles(t *testing.T) {
	sb := adotest.New(t)
	name, id := sb.Repository(t)
	pushExample(t, sb.URL(name), func(ctx context.Context) (gitx.Credentials, error) {
		pat, _, _ := sb.PAT(ctx)
		return gitx.Credentials{Header: "Authorization: " + azuredevops.BasicAuth(pat.Reveal())}, nil
	}, sb.Network.Proxy)
	h := start(t, t.TempDir(), `connector: azure-devops
  url: `+sb.URL(name)+`
  credentials: { ref: env:ADO_PAT }
  delivery: { mode: pull-request, merge: auto, mergeMethod: squash }
  sync: { minInterval: 1ns }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": sandboxPAT{sb}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"azure-devops": azuredevops.New}, HTTP: sb.HTTP},
		Network:    sb.Network,
	})
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	requireApproval(t, sb, repo, id)
	sharedFilesFlow(t, h, func(p string) string { return mirrorFile(t, h, p) })
}

// mirrorFile reads a file of the remote's main through the repository's mirror.
func mirrorFile(t *testing.T, h *harness, p string) string {
	t.Helper()
	ctx := context.Background()
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Mirror.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	head, err := repo.Mirror.Head(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	b, err := repo.Mirror.ReadFile(ctx, head, p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
