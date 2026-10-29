package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/adotest"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/azuredevops"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store/db"
)

const echoPipeline = "trigger: none\npool:\n  vmImage: ubuntu-latest\nsteps:\n  - script: echo quai\n"

// TestAzureDevOpsPipelineRuns follows the Azure Pipelines adapter on the sandbox
// (docs/design/06-runs-status-and-drift.md#adapters): a build validation pipeline's run on a pull
// request is a plan run of its stacks, read from the pull request's policy evaluations when
// polling; an apply pipeline's run, replayed from the pipeline-run subscription's history, is an
// apply run that finishes with the pipeline. Skipped without the sandbox's PAT.
func TestAzureDevOpsPipelineRuns(t *testing.T) {
	sb := adotest.New(t)
	ctx := context.Background()
	name, id := sb.Repository(t)
	if err := sb.AddFiles(ctx, id, "main", "Pipelines", map[string]string{"plan.yml": echoPipeline, "apply.yml": echoPipeline}); err != nil {
		t.Fatal(err)
	}
	planID, applyID := sb.Pipeline(t, name, id, "plan.yml"), sb.Pipeline(t, name, id, "apply.yml")
	e := start(t, name, `connector: azure-devops
  url: `+sb.URL(name)+`
  credentials: { ref: env:ADO_PAT }
  runner:
    pipelines:
      - { name: `+name+`-plan, kind: plan, stacks: envs/prod }
      - { name: `+name+`-apply, kind: apply, stacks: envs/prod }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": sandboxEnv{sb}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"azure-devops": azuredevops.New}, HTTP: sb.HTTP},
		Network:    sb.Network,
	})
	e.proxy = sb.Network.Proxy
	hook := "https://quai-ingest.invalid/webhooks/" + name
	if err := e.repo().Connector.Webhooks().Subscribe(ctx, hook, secrets.FromValue(hookSecret)); err != nil {
		t.Fatal(err)
	}
	sb.Unsubscribe(t, hook)
	if subs, _ := sb.Subscriptions(ctx, hook); subs["ms.vss-pipelines.run-state-changed-event"] == "" {
		t.Fatalf("no pipeline-run subscription: %v", subs)
	}
	runs := func(kind string) []db.Run {
		list, _ := e.store.ListRuns(ctx, db.ListRunsParams{Repository: name, Kind: kind, MaxRows: 10})
		return list
	}

	// The plan pipeline validates pull requests to main.
	settings := map[string]any{"buildDefinitionId": planID, "queueOnSourceUpdateOnly": false, "manualQueueOnly": false, "displayName": "plan",
		"validDuration": 0, "scope": []map[string]any{{"repositoryId": id, "refName": "refs/heads/main", "matchKind": "exact"}}}
	var policy struct {
		ID int `json:"id"`
	}
	if err := sb.Do(ctx, http.MethodPost, adotest.Path(sb.Project, "_apis", "policy", "configurations"), nil,
		map[string]any{"isEnabled": true, "isBlocking": true, "type": map[string]string{"id": "0609b952-1397-4640-95ec-e00a01b2c241"}, "settings": settings}, &policy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sb.Do(context.Background(), http.MethodDelete, adotest.Path(sb.Project, "_apis", "policy", "configurations", strconv.Itoa(policy.ID)), nil, nil, nil)
	})
	pr := e.openPR("quai/r-4-web01")
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if err := e.svc.Poll(ctx, name); err != nil {
			t.Fatal(err)
		}
		if plans := runs("plan"); len(plans) == 1 && plans[0].Status == "succeeded" {
			if p := plans[0]; p.PullRequest != pr.ID || p.Stack != "envs/prod" || p.Url == "" {
				t.Errorf("the plan run = %+v", p)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the build validation is not a succeeded plan run: %+v", runs("plan"))
		}
		time.Sleep(10 * time.Second)
	}

	// The apply pipeline runs on main; Azure DevOps cannot reach Quai: replay what it tried. A
	// subscription whose deliveries fail is put on probation and its next ones held back, so the
	// apply gets a fresh subscription: its first delivery names the apply's run, which Quai reads
	// again whatever state the event says.
	applyHook := "https://quai-ingest-apply.invalid/webhooks/" + name
	if err := e.repo().Connector.Webhooks().Subscribe(ctx, applyHook, secrets.FromValue(hookSecret)); err != nil {
		t.Fatal(err)
	}
	sb.Unsubscribe(t, applyHook)
	run, err := sb.Run(ctx, applyID, "main")
	if err != nil {
		t.Fatal(err)
	}
	sb.Await(t, applyID, run.ID, 10*time.Minute)
	deliveries := sb.DeliveriesWithin(t, 6*time.Minute, applyHook, azuredevops.SecretHeader, hookSecret, func(ds []adotest.Delivery) bool {
		for _, d := range ds {
			var e struct {
				Resource struct {
					Run struct {
						ID int `json:"id"`
					} `json:"run"`
				} `json:"resource"`
			}
			if d.EventType == "ms.vss-pipelines.run-state-changed-event" && json.Unmarshal(d.Body, &e) == nil && e.Resource.Run.ID == run.ID {
				return true
			}
		}
		return false
	})
	for _, d := range deliveries {
		if d.EventType == "ms.vss-pipelines.run-state-changed-event" {
			if status, out := e.send("/webhooks/"+name, delivery{d.Header, d.Body}); status != http.StatusAccepted {
				t.Errorf("%s: %d %v", d.EventType, status, out)
			}
		}
	}
	e.eventually("the apply is recorded", func() bool {
		applies := runs("apply")
		return len(applies) == 1 && applies[0].Status == "succeeded" && applies[0].PullRequest == "" && applies[0].Url == azuredevops.RunURL(sb.Collection, sb.Project, run.ID)
	})
	if plans := runs("plan"); len(plans) != 1 {
		t.Errorf("the plan's replayed run is another run: %+v", plans)
	}
	if len(e.handler.reported()) < 2 {
		t.Errorf("runs handed on = %+v", e.handler.reported())
	}
}
