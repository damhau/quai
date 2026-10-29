package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"text/template"

	"go.yaml.in/yaml/v3"
)

// manifests reads a YAML file of deploy/runners, one map per document.
func manifests(t *testing.T, file string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile("../../../deploy/runners/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			break
		}
		docs = append(docs, d)
	}
	if len(docs) == 0 {
		t.Fatalf("%s holds no YAML document", file)
	}
	return docs
}

// TestArgoCDTemplate renders the Argo CD notification template as the notifications controller
// would, for a sync running then finished, and Quai takes both as one sync run.
func TestArgoCDTemplate(t *testing.T) {
	h := newHarnessRunner(t, "")
	cm := manifests(t, "argocd/notifications.yaml")[0]["data"].(map[string]any)
	var tmpl struct {
		Webhook map[string]struct {
			Method, Path, Body string
		} `yaml:"webhook"`
	}
	if err := yaml.Unmarshal([]byte(cm["template.quai-sync"].(string)), &tmpl); err != nil {
		t.Fatal(err)
	}
	hook := tmpl.Webhook["quai"]
	if hook.Method != http.MethodPost || hook.Path != "/api/v1/runs" {
		t.Fatalf("the webhook = %+v", hook)
	}
	for _, key := range []string{"service.webhook.quai", "trigger.quai-sync"} {
		var v any
		if err := yaml.Unmarshal([]byte(cm[key].(string)), &v); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	body := template.Must(template.New("body").Parse(hook.Body))
	head := strings.TrimSpace(git(t, h.bare, "rev-parse", "main"))
	render := func(op map[string]any, annotations map[string]any) map[string]any {
		t.Helper()
		var b bytes.Buffer
		app := map[string]any{"metadata": map[string]any{"name": "shop", "namespace": "argocd", "annotations": annotations},
			"status": map[string]any{"sync": map[string]any{"revision": head}, "operationState": op}}
		if err := body.Execute(&b, map[string]any{"app": app, "context": map[string]any{"argocdUrl": "https://argocd.example.com"}}); err != nil {
			t.Fatal(err)
		}
		var report map[string]any
		if err := json.Unmarshal(b.Bytes(), &report); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, b.String())
		}
		return report
	}
	annotations := map[string]any{"quai.dev/repository": "infra-live", "quai.dev/stack": "envs/prod"}
	_, running, body1 := h.reportRun(runToken, render(map[string]any{"phase": "Running", "startedAt": "2026-10-29T10:00:00Z"}, annotations))
	_, done, body2 := h.reportRun(runToken, render(map[string]any{"phase": "Succeeded", "startedAt": "2026-10-29T10:00:00Z", "finishedAt": "2026-10-29T10:02:00Z"}, annotations))
	if running.Status != "running" || running.Kind != "sync" || running.Stack != "envs/prod" || done.ID != running.ID || done.Status != "succeeded" {
		t.Errorf("the sync = %s, then %s", body1, body2)
	}
	_, failed, _ := h.reportRun(runToken, render(map[string]any{"phase": "Failed", "startedAt": "2026-10-29T11:00:00Z", "finishedAt": "2026-10-29T11:01:00Z"},
		map[string]any{"quai.dev/repository": "infra-live"}))
	if failed.Status != "failed" || failed.Stack != "shop" || failed.ID == running.ID {
		t.Errorf("another sync, failed, of the Application = %+v", failed)
	}
}

// TestFluxEvents posts events as Flux's generic provider does: a reconciliation progressing then
// succeeded is one sync run; an error is a failed one.
func TestFluxEvents(t *testing.T) {
	h := newHarnessRunner(t, "")
	alert := manifests(t, "flux/alert.yaml")
	if address, _ := alert[0]["spec"].(map[string]any)["address"].(string); !strings.Contains(address, "/api/v1/runs/flux?repository=") {
		t.Errorf("the provider's address = %q", address)
	}
	head := strings.TrimSpace(git(t, h.bare, "rev-parse", "main"))
	send := func(token, query string, event map[string]any) (int, RunView, string) {
		t.Helper()
		b, _ := json.Marshal(event)
		req, _ := http.NewRequest(http.MethodPost, h.srv.URL+Prefix+"/runs/flux?"+query, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		var run RunView
		_ = json.Unmarshal(buf.Bytes(), &run)
		return resp.StatusCode, run, buf.String()
	}
	event := func(severity, reason, revision string) map[string]any {
		return map[string]any{
			"involvedObject": map[string]any{"kind": "Kustomization", "namespace": "flux-system", "name": "apps", "uid": "c1e2", "apiVersion": "kustomize.toolkit.fluxcd.io/v1"},
			"severity":       severity, "timestamp": "2026-10-29T10:00:00Z", "message": "Reconciliation finished", "reason": reason,
			"metadata":            map[string]any{"revision": revision, "summary": "apps"},
			"reportingController": "kustomize-controller", "reportingInstance": "kustomize-controller-1",
		}
	}
	status, progressing, body := send(runToken, "repository=infra-live", event("info", "Progressing", "main@sha1:"+head))
	if status != http.StatusAccepted || progressing.Status != "running" || progressing.Kind != "sync" || progressing.Stack != "flux-system/apps" || progressing.Commit != head {
		t.Fatalf("progressing = %d %s", status, body)
	}
	_, done, _ := send(runToken, "repository=infra-live", event("info", "ReconciliationSucceeded", "main@sha1:"+head))
	if done.ID != progressing.ID || done.Status != "succeeded" {
		t.Errorf("succeeded = %+v", done)
	}
	if _, failed, _ := send(runToken, "repository=infra-live&stack=envs/prod", event("error", "HealthCheckFailed", "main@sha1:abcdef1234")); failed.Status != "failed" || failed.Stack != "envs/prod" {
		t.Errorf("an error = %+v", failed)
	}
	if status, _, _ := send("wrong", "repository=infra-live", event("info", "ReconciliationSucceeded", "main@sha1:"+head)); status != http.StatusUnauthorized {
		t.Errorf("a wrong token = %d", status)
	}
	if status, _, body := send(runToken, "repository=infra-live", event("info", "ReconciliationSucceeded", "main")); status != http.StatusUnprocessableEntity || !strings.Contains(body, "names no commit") {
		t.Errorf("a revision without a commit = %d %s", status, body)
	}
}
