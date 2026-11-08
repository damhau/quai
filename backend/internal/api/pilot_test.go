package api

import (
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/config"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/giteatest"
)

// The pilot's types (P3-19), as they are on the stand-in pilot-infra: examples/pilot-infra.
const pilotExample = "../../../examples/pilot-infra"

// giteaPilot serves Quai on Gitea with the pilot's types and data beside the example repository,
// delivered by pull requests merged once approved, and the approvers of the pilot's rules: alice
// (platform-admins), nora (network) and sam (security).
func giteaPilot(t *testing.T) (*harness, *giteatest.Repo) {
	t.Helper()
	files := map[string]string{}
	src := os.DirFS(pilotExample)
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "README.md" {
			return err
		}
		b, err := fs.ReadFile(src, p)
		files[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return giteaHarnessOver(t, "auto", "", nil, files, giteaconn.New, func(cfg *config.Config) {
		cfg.Auth.Dev.Users = append(cfg.Auth.Dev.Users,
			config.DevUser{Name: "nora", Groups: []string{"network"}}, config.DevUser{Name: "sam", Groups: []string{"security"}})
		cfg.Auth.Roles = append(cfg.Auth.Roles,
			config.RoleMapping{Group: "network", Role: "viewer"}, config.RoleMapping{Group: "security", Role: "viewer"})
	})
}

// pilotFile reads a file of the pilot example.
func pilotFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(pilotExample + "/" + p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// request submits a request on a type of infra-live and waits for its pull request.
func (u *user) request(body map[string]any) RequestView {
	u.h.t.Helper()
	body["repository"], body["reason"] = "infra-live", "P3-19"
	var r RequestView
	if status, out := u.do(http.MethodPost, Prefix+"/requests/type", body, &r); status != http.StatusCreated {
		u.h.t.Fatalf("submit %v = %d %s", body, status, out)
	}
	return u.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
}

func (u *user) awaitMerged(id string) RequestView {
	u.h.t.Helper()
	return u.awaitDelivery(id, "merged", func(v RequestView) bool { return v.Delivery.MergeCommit != "" })
}

// view reads a request.
func (u *user) view(id string) RequestView {
	u.h.t.Helper()
	var r RequestView
	if status, body := u.do(http.MethodGet, Prefix+"/requests/"+id, nil, &r); status != http.StatusOK {
		u.h.t.Fatalf("request %s = %d %s", id, status, body)
	}
	return r
}

// ruleNames lists the names of the rules a request needs approved.
func ruleNames(r RequestView) []string {
	var names []string
	for _, rule := range r.Rules {
		names = append(names, rule.Names...)
	}
	return names
}

// mainFile reads a file of the Gitea repository's main branch.
func mainFile(t *testing.T, remote *giteatest.Repo, p string) string {
	t.Helper()
	b, _, err := giteatest.Get(t).Client.GetFile(remote.Owner, remote.Name, "main", p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPilotTypes: each of the pilot's types is requested through pull requests and merged once
// the teams its rules name approve: a namespace on a YAML list, its quota (a field of the same
// entries), a DNS record on an HCL map, and a firewall rule on a JSON list.
func TestPilotTypes(t *testing.T) {
	t.Parallel()
	h, remote := giteaPilot(t)
	bob, carol, alice, nora, sam := h.as("bob"), h.as("carol"), h.as("alice"), h.as("nora"), h.as("sam")

	// A new namespace on dev-1: the platform admins approve it.
	ns := bob.request(map[string]any{"type": "namespace", "kind": "create", "params": map[string]string{"cluster": "dev-1", "name": "search-dev"},
		"values": map[string]any{"owner": "bob", "description": "Search API", "quota": map[string]any{"cpu": 2, "memory_gi": 4}}})
	if names := ruleNames(ns); !slices.Equal(names, []string{"type/platform"}) {
		t.Errorf("the rules of a new namespace: %v", names)
	}
	if nora.view(ns.ID).CanReview {
		t.Error("the network team may approve a namespace")
	}
	alice.approve(ns.ID)
	bob.awaitMerged(ns.ID)

	// New labels on bob's namespace, the same quota: on dev-1, no one needs to approve.
	labels := bob.request(map[string]any{"type": "namespace", "kind": "update", "key": "dev-1/payments-dev",
		"values": map[string]any{"owner": "bob", "description": "Payment gateway", "labels": map[string]any{"app": "payments", "tier": "backend"},
			"quota": map[string]any{"cpu": 4, "memory_gi": 8}}})
	if len(labels.Rules) != 0 {
		t.Errorf("the rules of new labels on dev-1: %v", ruleNames(labels))
	}
	bob.awaitMerged(labels.ID)
	if got, want := mainFile(t, remote, "clusters/dev-1/namespaces.yaml"), strings.Replace(pilotFile(t, "clusters/dev-1/namespaces.yaml"),
		"      app: payments\n", "      app: payments\n      tier: backend\n", 1)+
		"  - name: search-dev\n    owner: bob\n    description: Search API\n    quota:\n      cpu: 2\n      memory_gi: 4\n"; got != want {
		t.Errorf("dev-1 after both requests:\n%s", got)
	}

	// More quota for payments on prod-1, asked by carol who does not own it: only the quota changes.
	quota := carol.request(map[string]any{"type": "namespace-quota", "kind": "update", "key": "prod-1/payments",
		"values": map[string]any{"quota": map[string]any{"cpu": 12, "memory_gi": 48}}})
	if names := ruleNames(quota); !slices.Equal(names, []string{"type/platform"}) {
		t.Errorf("the rules of a quota: %v", names)
	}
	alice.approve(quota.ID)
	carol.awaitMerged(quota.ID)
	if got, want := mainFile(t, remote, "clusters/prod-1/namespaces.yaml"), strings.Replace(pilotFile(t, "clusters/prod-1/namespaces.yaml"),
		"quota: { cpu: 8, memory_gi: 32 }", "quota: { cpu: 12, memory_gi: 48 }", 1); got != want {
		t.Errorf("prod-1 after the quota:\n%s", got)
	}

	// A CNAME leaving the internal zones: the network team and security approve it.
	cname := bob.request(map[string]any{"type": "dns-record", "kind": "create", "params": map[string]string{"zone": "booking.internal", "name": "mail"},
		"values": map[string]any{"owner": "bob", "type": "CNAME", "values": []string{"mx.mail-provider.example."}}})
	if names := ruleNames(cname); !slices.Equal(names, []string{"type/network", "type/external-alias"}) {
		t.Errorf("the rules of an external CNAME: %v", names)
	}
	nora.approve(cname.ID)
	if r := bob.view(cname.ID); r.State != "pending_approval" {
		t.Errorf("the CNAME approved by the network team only: %s", r.State)
	}
	sam.approve(cname.ID)
	bob.awaitMerged(cname.ID)
	if got, want := mainFile(t, remote, "stacks/dns/booking.internal/records.auto.tfvars"), strings.Replace(pilotFile(t, "stacks/dns/booking.internal/records.auto.tfvars"),
		"    values = [\"10.20.2.10\"]\n  }\n}\n",
		"    values = [\"10.20.2.10\"]\n  }\n  mail = {\n    owner  = \"bob\"\n    type   = \"CNAME\"\n    values = [\"mx.mail-provider.example.\"]\n  }\n}\n", 1); got != want {
		t.Errorf("booking.internal after the CNAME:\n%s", got)
	}

	// A rule in prod: security and the network team approve it.
	rule := bob.request(map[string]any{"type": "firewall-rule", "kind": "create", "params": map[string]string{"env": "prod", "name": "api-to-db"},
		"values": map[string]any{"owner": "bob", "protocol": "tcp", "sources": []string{"10.20.1.0/24"}, "destinations": []string{"10.20.3.10/32"}, "ports": []int{5432}}})
	if names := ruleNames(rule); !slices.Equal(names, []string{"type/security", "type/production-network"}) {
		t.Errorf("the rules of a rule in prod: %v", names)
	}
	sam.approve(rule.ID)
	nora.approve(rule.ID)
	bob.awaitMerged(rule.ID)
	got := mainFile(t, remote, "stacks/firewall/prod/rules.auto.tfvars.json")
	if want := strings.Replace(pilotFile(t, "stacks/firewall/prod/rules.auto.tfvars.json"), "    }\n  ]\n}\n",
		// New arrays go on several lines, whatever the style of the siblings' (docs/design/03, JSON).
		"    },\n    {\n      \"name\": \"api-to-db\",\n      \"owner\": \"bob\",\n      \"protocol\": \"tcp\",\n      \"sources\": [\n        \"10.20.1.0/24\"\n      ],\n"+
			"      \"destinations\": [\n        \"10.20.3.10/32\"\n      ],\n      \"ports\": [\n        5432\n      ]\n    }\n  ]\n}\n", 1); got != want {
		t.Errorf("prod's firewall after the rule:\n%s", got)
	}
}

// TestPilotConcurrentEntries: on each of the pilot's shared files, two requests creating two
// entries are both approved before either merges, and both merge. Both append to the collection,
// so the platform cannot merge the second after the first: Quai rebuilds it on the first, even
// while merging, and its approval stays (docs/design/03, concurrency; docs/design/05).
func TestPilotConcurrentEntries(t *testing.T) {
	t.Parallel()
	h, remote := giteaPilot(t)
	bob := h.as("bob")
	jsonRule := func(name, source string) string {
		return "    {\n      \"name\": \"" + name + "\",\n      \"owner\": \"bob\",\n      \"protocol\": \"tcp\",\n      \"sources\": [\n        \"" + source +
			"\"\n      ],\n      \"destinations\": [\n        \"10.120.9.10/32\"\n      ],\n      \"ports\": [\n        5432\n      ]\n    }"
	}
	for _, c := range []struct {
		name, approver, file string
		a, b                 map[string]any
		// with returns the file with the entries added, in the order they merged.
		with func(file string, entries ...string) string
		// entryA and entryB are what each request adds, in the file's style.
		entryA, entryB string
	}{
		{
			name: "YAML list", approver: "alice", file: "clusters/dev-1/namespaces.yaml",
			a: map[string]any{"type": "namespace", "kind": "create", "params": map[string]string{"cluster": "dev-1", "name": "search-dev"},
				"values": map[string]any{"owner": "bob", "quota": map[string]any{"cpu": 2, "memory_gi": 4}}},
			b: map[string]any{"type": "namespace", "kind": "create", "params": map[string]string{"cluster": "dev-1", "name": "loyalty-dev"},
				"values": map[string]any{"owner": "bob", "quota": map[string]any{"cpu": 1, "memory_gi": 2}}},
			with:   func(file string, entries ...string) string { return file + strings.Join(entries, "") },
			entryA: "  - name: search-dev\n    owner: bob\n    quota:\n      cpu: 2\n      memory_gi: 4\n",
			entryB: "  - name: loyalty-dev\n    owner: bob\n    quota:\n      cpu: 1\n      memory_gi: 2\n",
		},
		{
			name: "HCL map", approver: "nora", file: "stacks/dns/dev.booking.internal/records.auto.tfvars",
			a: map[string]any{"type": "dns-record", "kind": "create", "params": map[string]string{"zone": "dev.booking.internal", "name": "search"},
				"values": map[string]any{"owner": "bob", "type": "A", "values": []string{"10.120.3.10"}}},
			b: map[string]any{"type": "dns-record", "kind": "create", "params": map[string]string{"zone": "dev.booking.internal", "name": "loyalty"},
				"values": map[string]any{"owner": "bob", "type": "A", "values": []string{"10.120.4.10"}}},
			with: func(file string, entries ...string) string {
				return strings.TrimSuffix(file, "}\n") + strings.Join(entries, "") + "}\n"
			},
			entryA: "  search   = { owner = \"bob\", type = \"A\", values = [\"10.120.3.10\"] }\n",
			entryB: "  loyalty  = { owner = \"bob\", type = \"A\", values = [\"10.120.4.10\"] }\n",
		},
		{
			name: "JSON list", approver: "sam", file: "stacks/firewall/dev/rules.auto.tfvars.json",
			a: map[string]any{"type": "firewall-rule", "kind": "create", "params": map[string]string{"env": "dev", "name": "search-to-db"},
				"values": map[string]any{"owner": "bob", "protocol": "tcp", "sources": []string{"10.120.3.0/24"}, "destinations": []string{"10.120.9.10/32"}, "ports": []int{5432}}},
			b: map[string]any{"type": "firewall-rule", "kind": "create", "params": map[string]string{"env": "dev", "name": "loyalty-to-db"},
				"values": map[string]any{"owner": "bob", "protocol": "tcp", "sources": []string{"10.120.4.0/24"}, "destinations": []string{"10.120.9.10/32"}, "ports": []int{5432}}},
			with: func(file string, entries ...string) string {
				return strings.TrimSuffix(file, "\n  ]\n}\n") + ",\n" + strings.Join(entries, ",\n") + "\n  ]\n}\n"
			},
			entryA: jsonRule("search-to-db", "10.120.3.0/24"),
			entryB: jsonRule("loyalty-to-db", "10.120.4.0/24"),
		},
	} {
		approver := h.as(c.approver)
		a, b := bob.request(c.a), bob.request(c.b)
		approver.approve(a.ID)
		approver.approve(b.ID)
		a, b = bob.awaitMerged(a.ID), bob.awaitMerged(b.ID)
		// The second to merge was rebuilt on the first, and merged with the approval it had.
		second := b
		if hasEvent(a, "rebuilt", "") {
			second = a
		}
		if !hasEvent(second, "rebuilt", "") || len(second.Reviews) != 1 {
			t.Errorf("%s: the second request to merge: %+v, reviews %+v", c.name, second.Events, second.Reviews)
		}
		original := pilotFile(t, c.file)
		got := mainFile(t, remote, c.file)
		if got != c.with(original, c.entryA, c.entryB) && got != c.with(original, c.entryB, c.entryA) {
			t.Errorf("%s: %s after both:\n%s", c.name, c.file, got)
		}
	}
}
