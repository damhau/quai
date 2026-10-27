package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// Ported from "sync webhook updates deployment status": runners report runs with the
// repository's run token, and resources show their stack's latest apply.
func TestRunReports(t *testing.T) {
	h := newHarness(t)
	token := "run-token-for-infra-live"
	sum := sha256.Sum256([]byte(token))
	repo, _ := h.reg.Get("infra-live")
	repo.RunTokenSHA256 = hex.EncodeToString(sum[:])
	runner := h.as("")

	send := func(tok string) int {
		req, _ := http.NewRequest(http.MethodPost, h.srv.URL+Prefix+"/runs", strings.NewReader(`{"repository":"infra-live","stack":"envs/prod","commit":"abcdef1","kind":"apply","status":"succeeded","url":"https://ci.example.com/1"}`))
		req.Header.Set("Content-Type", "application/json")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := runner.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := send("wrong"); status != http.StatusUnauthorized {
		t.Errorf("wrong token = %d", status)
	}
	if status := send(""); status != http.StatusUnprocessableEntity {
		t.Errorf("no token = %d", status)
	}
	if status := send(token); status != http.StatusAccepted {
		t.Fatalf("report = %d", status)
	}
	var d ResourceDetail
	h.as("bob").do(http.MethodGet, Prefix+"/resource?id=infra-live/vm/prod/web01", nil, &d)
	if d.LastApply == nil || d.LastApply.Status != "succeeded" || d.LastApply.Commit != "abcdef1" {
		t.Errorf("last apply = %+v", d.LastApply)
	}
	var inv []ResourceView
	h.as("bob").do(http.MethodGet, Prefix+"/inventory", nil, &inv)
	for _, r := range inv {
		if (r.Stack == "envs/prod") != (r.LastApply != nil) {
			t.Errorf("%s: last apply %+v", r.ID, r.LastApply)
		}
	}
}

func TestInstancesLogAndEvents(t *testing.T) {
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
