package ingest

import (
	"context"
	"net/http"
	"testing"

	"github.com/damhau/quai/backend/internal/adotest"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/azuredevops"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// TestAzureDevOpsReplay replays to Quai what Azure DevOps tried to deliver for a push and a new
// pull request (it cannot reach this machine): verified, recorded, and processed by re-reading
// the pull request from the sandbox. Skipped without the sandbox's PAT.
func TestAzureDevOpsReplay(t *testing.T) {
	sb := adotest.New(t)
	ctx := context.Background()
	name, _ := sb.Repository(t)
	e := start(t, name, `connector: azure-devops
  url: `+sb.URL(name)+`
  credentials: { ref: env:ADO_PAT }`, repositories.Options{
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

	pr := e.openPR("quai/r-3-web01")
	deliveries := sb.Deliveries(t, hook, azuredevops.SecretHeader, hookSecret, func(ds []adotest.Delivery) bool {
		return adotest.HasEvent(ds, "git.push") && adotest.HasEvent(ds, "git.pullrequest.created")
	})
	for _, d := range deliveries {
		if status, out := e.send("/webhooks/"+name, delivery{d.Header, d.Body}); status != http.StatusAccepted || out["duplicate"] == true {
			t.Errorf("%s: %d %v", d.EventType, status, out)
		}
	}
	e.eventually("the pull request is re-read from the sandbox", func() bool { return e.handler.seen(pr.ID, connectors.PROpen) })
	if s, err := e.store.GetRepositorySync(ctx, name); err != nil || !s.LastWebhookAt.Valid || !s.LastFetchAt.Valid || s.Head == "" {
		t.Errorf("sync = %+v, %v", s, err)
	}
	// Replayed again: duplicates.
	if status, out := e.send("/webhooks/"+name, delivery{deliveries[0].Header, deliveries[0].Body}); status != http.StatusAccepted || out["duplicate"] != true {
		t.Errorf("a redelivery: %d %v", status, out)
	}
	forged := deliveries[0].Header.Clone()
	forged.Set(azuredevops.SecretHeader, "wrong")
	if status, _ := e.send("/webhooks/"+name, delivery{forged, deliveries[0].Body}); status != http.StatusUnauthorized {
		t.Errorf("a wrong secret = %d", status)
	}
}

// sandboxEnv resolves env:ADO_PAT to the sandbox's PAT, and env:HOOK_SECRET.
type sandboxEnv struct{ sb *adotest.Sandbox }

func (e sandboxEnv) Get(ctx context.Context, name string) (secrets.Secret, error) {
	if name == "ADO_PAT" {
		return e.sb.Get(ctx, name)
	}
	return secrets.Env{Lookup: func(n string) (string, bool) { return hookSecret, n == "HOOK_SECRET" }}.Get(ctx, name)
}
