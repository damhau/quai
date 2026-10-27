package api

import (
	"net/http"
	"strings"
	"testing"
)

func newVM(env, name string, cpu int) map[string]any {
	return map[string]any{
		"repository": "infra-live", "type": "vm", "kind": "create",
		"params": map[string]string{"env": env, "name": name},
		"values": map[string]any{"template": "rhel-9", "cpu": cpu, "memory_gb": 8, "disks": []any{map[string]any{"size_gb": 50}}, "network": map[string]any{"vlan": 110}},
	}
}

// Ported from "create writes one file at the type's path, owned by the requester".
func TestCreateIsCommittedWhenNoRuleApplies(t *testing.T) {
	h := newHarness(t)
	bob := h.as("bob")
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", newVM("dev", "shop-app01", 2), &r); status != http.StatusCreated {
		t.Fatalf("request = %d %s", status, body)
	}
	if r.State != "approved" || r.Target != "infra-live/vm/dev/shop-app01" || r.Stack != "envs/dev" || len(r.Rules) != 0 || r.Changes[0].Op != "create" {
		t.Errorf("request = %+v", r)
	}
	done := bob.await(r.ID, "applied")
	content, ok := h.file("envs/dev/vms/shop-app01.yaml")
	if !ok || !strings.HasPrefix(content, "owner: bob\ntemplate: rhel-9\ncpu: 2\n") {
		t.Errorf("the file in Git = %q", content)
	}
	log := git(t, "", "--git-dir", h.bare, "log", "-1", "--format=%an|%cn|%s|%(trailers:key=Quai-Request,valueonly)|%(trailers:key=Quai-Stack,valueonly)", "main")
	if !strings.HasPrefix(log, "bob|Quai|quai: create infra-live/vm/dev/shop-app01|#") || !strings.Contains(log, "envs/dev") {
		t.Errorf("the commit = %q", log)
	}
	kinds := []string{}
	for _, e := range done.Events {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "submitted,approved,merging,committed" {
		t.Errorf("events = %v", kinds)
	}
	// The same resource again is a conflict.
	if status, _ := bob.do(http.MethodPost, Prefix+"/requests/type", newVM("dev", "shop-app01", 2), nil); status != http.StatusConflict {
		t.Errorf("creating it twice = %d", status)
	}
}

// Ported from "create validates params and the document against the repo's JSON Schema".
func TestCreateValidation(t *testing.T) {
	h := newHarness(t)
	bob := h.as("bob")
	req := newVM("staging", "Bad_Name", 99)
	status, body := bob.do(http.MethodPost, Prefix+"/plans/type", req, nil)
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, `"body.params.env"`) || !strings.Contains(body, `"body.params.name"`) {
		t.Errorf("bad params = %d %s", status, body)
	}
	req = newVM("dev", "ok-name", 99)
	status, body = bob.do(http.MethodPost, Prefix+"/plans/type", req, nil)
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, `"location":"body.values.cpu"`) || !strings.Contains(body, "must be at most 32") {
		t.Errorf("schema violation = %d %s", status, body)
	}
}

// Ported from "prod create needs platform-admins; growing past 8 vCPU needs finops" and
// "requester cannot self-approve".
func TestApprovals(t *testing.T) {
	h := newHarness(t)
	bob, alice, fiona, carol := h.as("bob"), h.as("alice"), h.as("fiona"), h.as("carol")
	var p PlanView
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/type", newVM("prod", "big01", 16), &p); status != http.StatusOK || len(p.Rules) != 2 {
		t.Fatalf("plan = %d %s", status, body)
	}
	// The floor rule and the type's production rule ask the same group: one requirement.
	if strings.Join(p.Rules[0].Names, ",") != "floor/production-floor,type/production" || p.Rules[1].Approvers[0] != "group:finops" {
		t.Errorf("rules = %+v", p.Rules)
	}
	var r RequestView
	bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "big01", 16), &r)
	if r.State != "pending_approval" {
		t.Fatalf("state = %s", r.State)
	}
	if status, _ := bob.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusForbidden {
		t.Errorf("self-approval = %d", status)
	}
	if status, _ := carol.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusForbidden {
		t.Errorf("approval by someone not in a rule = %d", status)
	}
	var inbox []RequestView
	alice.do(http.MethodGet, Prefix+"/requests?view=inbox", nil, &inbox)
	if len(inbox) != 1 || !inbox[0].CanReview {
		t.Errorf("alice's inbox = %+v", inbox)
	}
	alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, &r)
	if r.State != "pending_approval" || !r.Rules[0].Satisfied || r.Rules[1].Satisfied {
		t.Errorf("after alice = %+v", r)
	}
	fiona.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, &r)
	if r.State != "approved" {
		t.Errorf("after fiona = %s", r.State)
	}
	bob.await(r.ID, "applied")
	if _, ok := h.file("envs/prod/vms/big01.yaml"); !ok {
		t.Error("the approved VM is not in Git")
	}
	var mine []RequestView
	bob.do(http.MethodGet, Prefix+"/requests?view=mine", nil, &mine)
	if len(mine) != 1 || len(mine[0].Rules) != 2 {
		t.Errorf("bob's requests = %+v", mine)
	}
}

// Ported from "denial rejects and requires a comment".
func TestDenialAndCancel(t *testing.T) {
	h := newHarness(t)
	bob, alice := h.as("bob"), h.as("alice")
	var r RequestView
	bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "web09", 2), &r)
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "deny"}, nil); status != http.StatusUnprocessableEntity || !strings.Contains(body, "body.comment") {
		t.Errorf("denial without a comment = %d %s", status, body)
	}
	alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "deny", "comment": "use VLAN 120"}, &r)
	if r.State != "rejected" || r.Reviews[0].Comment != "use VLAN 120" {
		t.Errorf("denied = %+v", r)
	}
	if status, _ := bob.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/cancel", nil, nil); status != http.StatusConflict {
		t.Errorf("cancel a rejected request = %d", status)
	}
	bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "web09", 2), &r)
	if status, _ := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/cancel", nil, nil); status != http.StatusOK {
		t.Errorf("an admin cancels = %d", status)
	}
	bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "web09", 2), &r)
	if status, _ := h.as("carol").do(http.MethodPost, Prefix+"/requests/"+r.ID+"/cancel", nil, nil); status != http.StatusForbidden {
		t.Errorf("someone else cancels = %d", status)
	}
	bob.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/cancel", nil, &r)
	if r.State != "cancelled" {
		t.Errorf("cancelled = %s", r.State)
	}
	if status, _ := bob.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/retry", nil, nil); status != http.StatusConflict {
		t.Errorf("retry a cancelled request = %d", status)
	}
}

// Ported from "day-2 resize on a hand-written file changes only the touched lines" and "indexed
// set paths, immutable fields, owner protection and no-ops".
func TestDayTwo(t *testing.T) {
	h := newHarness(t)
	bob := h.as("bob")
	resize := map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": "prod/web01", "action": "resize", "actionInputs": map[string]any{"cpu": "8", "memory_gb": 32}}
	var p PlanView
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/type", resize, &p); status != http.StatusOK {
		t.Fatalf("plan = %d %s", status, body)
	}
	var changed []string
	for _, l := range p.Changes[0].Diff {
		if l.Op != " " {
			changed = append(changed, l.Op+l.Text)
		}
	}
	if strings.Join(changed, "|") != "-cpu: 4          # bumped in INC-4411|-memory_gb: 16|+cpu: 8          # bumped in INC-4411|+memory_gb: 32" {
		t.Errorf("diff = %q", changed)
	}
	var r RequestView
	bob.do(http.MethodPost, Prefix+"/requests/type", resize, &r) // prod: needs an admin
	h.as("alice").do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil)
	bob.await(r.ID, "applied")
	if content, _ := h.file("envs/prod/vms/web01.yaml"); !strings.Contains(content, "cpu: 8          # bumped in INC-4411\nmemory_gb: 32\n") {
		t.Errorf("web01 = %q", content)
	}

	// Indexed set paths: extend-disk sets disks.0.size_gb on bob's dev VM, which needs no approval.
	extend := map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": "dev/build-agent-01", "action": "extend-disk", "actionInputs": map[string]any{"size_gb": 120}}
	if status, body := h.as("carol").do(http.MethodPost, Prefix+"/plans/type", extend, nil); status != http.StatusForbidden {
		t.Errorf("carol extending bob's disk = %d %s", status, body)
	}
	bob.do(http.MethodPost, Prefix+"/requests/type", extend, &r)
	bob.await(r.ID, "applied")
	if content, _ := h.file("envs/dev/vms/build-agent-01.yaml"); !strings.Contains(content, "- size_gb: 120") {
		t.Errorf("build-agent-01 = %q", content)
	}
	immutable := map[string]any{"repository": "infra-live", "type": "vm", "kind": "update", "key": "prod/web01",
		"values": map[string]any{"template": "rhel-9", "cpu": 8, "memory_gb": 32, "disks": []any{map[string]any{"size_gb": 80, "tier": "ssd"}}, "network": map[string]any{"vlan": 120, "ip": "10.0.12.11"}}}
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/type", immutable, nil); status != http.StatusUnprocessableEntity || !strings.Contains(body, `"location":"body.values.template"`) {
		t.Errorf("changing an immutable field = %d %s", status, body)
	}
	noop := map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": "prod/web01", "action": "resize", "actionInputs": map[string]any{"cpu": 8, "memory_gb": 32}}
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/type", noop, nil); status != http.StatusUnprocessableEntity || !strings.Contains(body, "would not modify") {
		t.Errorf("a no-op = %d %s", status, body)
	}
	// Ported from "only owners, operators or admins can change a resource": db01 is dana's.
	other := map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": "prod/db01", "action": "resize", "actionInputs": map[string]any{"cpu": 16, "memory_gb": 64}}
	if status, _ := bob.do(http.MethodPost, Prefix+"/plans/type", other, nil); status != http.StatusForbidden {
		t.Errorf("bob resizing dana's VM = %d", status)
	}
	if status, _ := h.as("alice").do(http.MethodPost, Prefix+"/plans/type", other, nil); status != http.StatusOK {
		t.Errorf("an admin resizing dana's VM = %d", status)
	}
	if status, _ := h.as("fiona").do(http.MethodPost, Prefix+"/plans/type", resize, nil); status != http.StatusForbidden {
		t.Errorf("a viewer resizing = %d", status)
	}
}

// Ported from "a human edit to the file makes a pending approval stale".
func TestStale(t *testing.T) {
	h := newHarness(t)
	bob, alice := h.as("bob"), h.as("alice")
	var r RequestView
	bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": "prod/web01",
		"action": "resize", "actionInputs": map[string]any{"cpu": 6, "memory_gb": 16}}, &r)
	h.push("envs/prod/vms/web01.yaml", "owner: bob\ntemplate: ubuntu-24.04\ncpu: 5\nmemory_gb: 16\ndisks: [{ size_gb: 80 }]\nnetwork: { vlan: 120 }\n")
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, &r); status != http.StatusOK || r.State != "stale" {
		t.Errorf("approving after a hand edit = %d %s", status, body)
	}
	if status, _ := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusConflict {
		t.Errorf("approving a stale request = %d", status)
	}
}

// Ported from "delete removes the file; unparseable files are listed but never edited".
func TestDeleteAndUnparseable(t *testing.T) {
	h := newHarness(t)
	h.push("envs/dev/vms/broken.yaml", "owner: bob\ncpu: [2\n")
	bob, alice := h.as("bob"), h.as("alice")
	var r RequestView
	del := map[string]any{"repository": "infra-live", "type": "vm", "kind": "delete", "key": "dev/build-agent-01", "reason": "decommissioned"}
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", del, &r); status != http.StatusCreated {
		t.Fatalf("delete = %d %s", status, body)
	}
	// Deleting needs an approval from the owners or the floor rules' approvers, even in dev.
	if r.State != "pending_approval" || len(r.Rules) != 1 || strings.Join(r.Rules[0].Approvers, ",") != "group:platform-admins,owners" {
		t.Fatalf("delete request = %+v", r)
	}
	if status, _ := h.as("carol").do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusForbidden {
		t.Errorf("carol approving = %d", status)
	}
	alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil)
	bob.await(r.ID, "applied")
	if _, ok := h.file("envs/dev/vms/build-agent-01.yaml"); ok {
		t.Error("the deleted VM is still in Git")
	}
	var inv []ResourceView
	bob.do(http.MethodGet, Prefix+"/inventory", nil, &inv)
	for _, res := range inv {
		if res.Key == "dev/build-agent-01" {
			t.Error("the deleted VM is still in the inventory")
		}
		if res.Key == "dev/broken" && res.Status != "unparseable" {
			t.Errorf("broken.yaml = %+v", res)
		}
	}
	broken := map[string]any{"repository": "infra-live", "type": "vm", "kind": "update", "key": "dev/broken", "values": map[string]any{}}
	if status, body := alice.do(http.MethodPost, Prefix+"/plans/type", broken, nil); status != http.StatusConflict || !strings.Contains(body, "unparseable") {
		t.Errorf("editing an unparseable file = %d %s", status, body)
	}
}

func TestBlueprintRequest(t *testing.T) {
	h := newHarness(t)
	bob := h.as("bob")
	var r RequestView
	in := map[string]any{"repository": "infra-live", "blueprint": "app-servers", "name": "shop-dev", "inputs": map[string]any{"environment": "dev", "app": "shop"}, "reason": "shop dev"}
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/blueprint", in, &r); status != http.StatusCreated || r.State != "approved" || len(r.Changes) != 3 {
		t.Fatalf("blueprint request = %d %s", status, body)
	}
	bob.await(r.ID, "applied")
	for _, f := range []string{"envs/dev/vms/shop-web01.yaml", "envs/dev/vms/shop-db01.yaml", ".quai/instances/app-servers/shop-dev.yaml"} {
		if _, ok := h.file(f); !ok {
			t.Errorf("%s is not in Git", f)
		}
	}
	var p PlanView
	in = map[string]any{"repository": "infra-live", "blueprint": "linux-vm", "name": "big", "inputs": map[string]any{"environment": "prod", "name": "big01", "size": "large"}}
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/blueprint", in, &p); status != http.StatusOK || len(p.Rules) != 2 {
		t.Errorf("prod large blueprint plan = %d %s", status, body)
	}
	if status, _ := bob.do(http.MethodPost, Prefix+"/plans/blueprint", map[string]any{"repository": "infra-live", "blueprint": "nope", "name": "x"}, nil); status != http.StatusNotFound {
		t.Errorf("unknown blueprint = %d", status)
	}
}
