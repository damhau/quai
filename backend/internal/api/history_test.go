package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestInstancesLogAndEvents(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	bob := h.as("bob")
	var r RequestView
	bob.do(http.MethodPost, Prefix+"/requests/blueprint", map[string]any{"repository": "infra-live", "blueprint": "app-servers", "name": "shop-dev",
		"inputs": map[string]any{"environment": "dev", "app": "shop"}}, &r)
	bob.await(r.ID, "applied")
	h.push("envs/dev/vms/shop-db01.yaml", "") // someone empties the database VM by hand
	git(t, h.work, "rm", "--quiet", "envs/dev/vms/shop-db01.yaml")
	git(t, h.work, "commit", "--quiet", "-m", "remove the database by hand")
	git(t, h.work, "push", "--quiet", "origin", "HEAD:main")
	bob.do(http.MethodPost, Prefix+"/repositories/infra-live/sync", nil, nil)

	var list []InstanceView
	if status, body := bob.do(http.MethodGet, Prefix+"/instances", nil, &list); status != http.StatusOK || len(list) != 1 || list[0].Name != "shop-dev" {
		t.Fatalf("instances = %d %s", status, body)
	}
	var inst InstanceView
	if status, _ := bob.do(http.MethodGet, Prefix+"/instances/infra-live/app-servers/shop-dev", nil, &inst); status != http.StatusOK || len(inst.Resources) != 2 {
		t.Fatalf("instance = %d %+v", status, inst)
	}
	if inst.Resources[0].ID != "db" || inst.Resources[0].Exists || !inst.Resources[1].Exists || inst.Inputs["app"] != "shop" {
		t.Errorf("instance resources = %+v", inst)
	}
	if status, _ := bob.do(http.MethodGet, Prefix+"/instances/infra-live/app-servers/nope", nil, nil); status != http.StatusNotFound {
		t.Errorf("missing instance = %d", status)
	}
	if !inst.CanOperate || len(inst.Requests) != 1 || inst.Requests[0].ID != r.ID {
		t.Errorf("the instance's requests = %v, can operate = %v", inst.Requests, inst.CanOperate)
	}

	var log []CommitView
	if status, _ := bob.do(http.MethodGet, Prefix+"/repositories/infra-live/log?limit=10", nil, &log); status != http.StatusOK || len(log) < 3 || log[0].Subject != "remove the database by hand" {
		t.Errorf("log = %d %+v", status, log)
	}
	var commit struct{ SHA, Text string }
	if status, _ := bob.do(http.MethodGet, Prefix+"/repositories/infra-live/commits/"+log[2].SHA, nil, &commit); status != http.StatusOK || !strings.Contains(commit.Text, "quai: create app-servers/shop-dev") {
		t.Errorf("commit = %d %q", status, commit.Text)
	}
	if status, _ := bob.do(http.MethodGet, Prefix+"/repositories/infra-live/commits/0000000", nil, nil); status != http.StatusNotFound {
		t.Errorf("unknown commit = %d", status)
	}
	var events []EventView
	if status, _ := bob.do(http.MethodGet, Prefix+"/events?limit=5", nil, &events); status != http.StatusOK || len(events) != 4 || events[0].Kind != "committed" {
		t.Errorf("events = %d %+v", status, events)
	}
}

// Ported from "only admins publish blueprints, and they are validated and committed".
func TestPublishBlueprint(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	bp := map[string]any{
		"apiVersion": "quai.dev/v1beta1", "kind": "Blueprint",
		"metadata": map[string]any{"name": "web-pair", "title": "Two web servers"},
		"spec": map[string]any{
			"inputs":    map[string]any{"type": "object", "properties": map[string]any{"env": map[string]any{"type": "string", "enum": []any{"dev", "prod"}}}},
			"resources": []any{map[string]any{"id": "a", "type": map[string]any{"name": "vm"}, "params": map[string]any{"env": "${{ inputs.env }}", "name": "web-a"}}},
		},
	}
	if status, _ := h.as("bob").do(http.MethodPut, Prefix+"/repositories/infra-live/blueprints/web-pair", map[string]any{"blueprint": bp}, nil); status != http.StatusForbidden {
		t.Errorf("bob publishing = %d", status)
	}
	alice := h.as("alice")
	bad := map[string]any{"apiVersion": "quai.dev/v1beta1", "kind": "Blueprint", "metadata": map[string]any{"name": "web-pair", "title": "x"},
		"spec": map[string]any{"inputs": map[string]any{}, "resources": []any{map[string]any{"id": "a", "type": map[string]any{"name": "nope"}}}}}
	if status, body := alice.do(http.MethodPut, Prefix+"/repositories/infra-live/blueprints/web-pair", map[string]any{"blueprint": bad}, nil); status != http.StatusUnprocessableEntity || !strings.Contains(body, "type nope is not defined") {
		t.Errorf("an invalid blueprint = %d %s", status, body)
	}
	if status, _ := alice.do(http.MethodPut, Prefix+"/repositories/infra-live/blueprints/other", map[string]any{"blueprint": bp}, nil); status != http.StatusUnprocessableEntity {
		t.Errorf("a name mismatch = %d", status)
	}
	var v BlueprintView
	if status, body := alice.do(http.MethodPut, Prefix+"/repositories/infra-live/blueprints/web-pair", map[string]any{"blueprint": bp}, &v); status != http.StatusOK || v.Name != "web-pair" {
		t.Fatalf("publish = %d %s", status, body)
	}
	content, ok := h.file("blueprints/web-pair.yaml")
	if !ok || !strings.HasPrefix(content, "apiVersion: quai.dev/v1beta1\nkind: Blueprint\nmetadata:\n  name: web-pair\n") {
		t.Errorf("blueprints/web-pair.yaml = %q", content)
	}
	var items []CatalogItem
	alice.do(http.MethodGet, Prefix+"/catalog", nil, &items)
	if len(items) != 4 {
		t.Errorf("catalog after publishing = %d items", len(items))
	}
	// Publishing the same again changes nothing.
	if status, _ := alice.do(http.MethodPut, Prefix+"/repositories/infra-live/blueprints/web-pair", map[string]any{"blueprint": bp}, nil); status != http.StatusOK {
		t.Errorf("publishing again = %d", status)
	}
}

func TestInstanceActions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	bob := h.as("bob")
	var r RequestView
	bob.do(http.MethodPost, Prefix+"/requests/blueprint", map[string]any{"repository": "infra-live", "blueprint": "app-servers", "name": "shop-dev",
		"inputs": map[string]any{"environment": "dev", "app": "shop", "withDatabase": false}}, &r)
	bob.await(r.ID, "applied")

	scale := map[string]any{"repository": "infra-live", "blueprint": "app-servers", "instance": "shop-dev", "action": "scale-web", "actionInputs": map[string]any{"cpu": 64}}
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/instance-action", scale, nil); status != http.StatusUnprocessableEntity || !strings.Contains(body, `"location":"body.actionInputs.cpu"`) {
		t.Errorf("an input out of range = %d %s", status, body)
	}
	scale["actionInputs"] = map[string]any{"cpu": 4}
	var p PlanView
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/instance-action", scale, &p); status != http.StatusOK || len(p.Changes) != 1 || p.Changes[0].Path != "envs/dev/vms/shop-web01.yaml" || p.Target != "app-servers/shop-dev" {
		t.Fatalf("plan = %d %s", status, body)
	}
	if status, _ := h.as("carol").do(http.MethodPost, Prefix+"/plans/instance-action", scale, nil); status != http.StatusForbidden {
		t.Errorf("someone else's instance = %d", status)
	}
	bob.do(http.MethodPost, Prefix+"/requests/instance-action", map[string]any{"repository": "infra-live", "blueprint": "app-servers", "instance": "shop-dev",
		"action": "scale-web", "actionInputs": map[string]any{"cpu": 4}, "reason": "Black Friday"}, &r)
	bob.await(r.ID, "applied")
	if content, _ := h.file("envs/dev/vms/shop-web01.yaml"); !strings.Contains(content, "cpu: 4\nmemory_gb: 8\n") {
		t.Errorf("shop-web01 = %q", content)
	}
	if r.Kind != "action" || r.Action != "scale-web" || r.Blueprint != "app-servers" {
		t.Errorf("request = %+v", r)
	}
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/instance-action", scale, nil); status != http.StatusUnprocessableEntity || !strings.Contains(body, "would not modify") {
		t.Errorf("the same action again = %d %s", status, body)
	}
	scale["instance"] = "nope"
	if status, _ := bob.do(http.MethodPost, Prefix+"/plans/instance-action", scale, nil); status != http.StatusNotFound {
		t.Errorf("an unknown instance = %d", status)
	}
}
