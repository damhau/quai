package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/adotest"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/azuredevops"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// sandboxPAT resolves env:ADO_PAT to the sandbox's PAT.
type sandboxPAT struct{ sb *adotest.Sandbox }

func (p sandboxPAT) Get(ctx context.Context, name string) (secrets.Secret, error) {
	if name != "ADO_PAT" {
		return secrets.Secret{}, secrets.ErrNotFound
	}
	return p.sb.Get(ctx, name)
}

// TestAzureDevOpsDelivery: on the sandbox, a request opens a pull request with quai/approval
// pending, required by a status check policy that only Quai's identity satisfies; the approval
// sets the status and auto-complete, Azure DevOps completes the pull request, and the poll
// applies the request. Skipped without the sandbox's PAT.
func TestAzureDevOpsDelivery(t *testing.T) {
	sb := adotest.New(t)
	ctx := context.Background()
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
	me, err := repo.Connector.(*azuredevops.Connector).Client().Detect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	policy := map[string]any{"isEnabled": true, "isBlocking": true, "type": map[string]string{"id": "cbdc66da-9728-4af8-aada-9a5a32e4a226"},
		"settings": map[string]any{"statusGenre": "quai", "statusName": "approval", "authorId": me.User.ID, "invalidateOnSourceUpdate": true,
			"scope": []map[string]any{{"repositoryId": id, "refName": "refs/heads/main", "matchKind": "exact"}}}}
	if err := sb.Do(ctx, http.MethodPost, adotest.Path(sb.Project, "_apis", "policy", "configurations"), nil, policy, nil); err != nil {
		t.Fatal(err)
	}

	bob, alice := h.as("bob"), h.as("alice")
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resizeBody(6), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	pr, err := repo.Connector.PullRequests().Get(ctx, r.Delivery.PullRequest)
	if err != nil || pr.Source != "quai/r-1-web01-resize" || !strings.Contains(pr.Description, connectors.Marker(r.ID)) || len(pr.Labels) == 0 {
		t.Errorf("the pull request: source %q, labels %v, marker %v, %v", pr.Source, pr.Labels, strings.Contains(pr.Description, connectors.Marker(r.ID)), err)
	}
	statuses, err := repo.Connector.Statuses().List(ctx, connectors.StatusTarget{PullRequest: pr.ID})
	if err != nil || len(statuses) == 0 || statuses[0].Name != "quai/approval" || statuses[0].State != connectors.StatusPending {
		t.Errorf("statuses: %+v, %v", statuses, err)
	}

	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("approve = %d %s", status, body)
	}
	r = bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if r.Delivery.MergeCommit == "" {
		t.Errorf("delivery = %+v", r.Delivery)
	}
}
