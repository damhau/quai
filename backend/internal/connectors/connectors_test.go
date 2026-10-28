package connectors_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/generic"
)

func TestRegistry(t *testing.T) {
	var logs strings.Builder
	r := &connectors.Registry{
		Factories: map[string]connectors.Factory{"generic": generic.New},
		Fallback:  generic.Fallback,
		Log:       slog.New(slog.NewTextHandler(&logs, nil)),
	}
	c, err := r.New("generic", connectors.Config{URL: "/srv/git/infra.git"})
	if err != nil || c.Info().Platform != "generic" || c.PullRequests() != nil {
		t.Fatalf("generic = %+v, %v", c, err)
	}
	// Without credentials, git authenticates with nothing.
	if creds, err := c.GitAuth(context.Background()); err != nil || creds.Header != "" || creds.SSHKey != nil {
		t.Errorf("GitAuth = %+v, %v", creds, err)
	}

	// A platform whose connector is not built yet gets Git transport only, and a warning.
	c, err = r.New("gitea", connectors.Config{URL: "https://git.example.com/quai/infra.git"})
	if err != nil || c.Info().Platform != "gitea" || c.Info().Has(connectors.CapPullRequests) {
		t.Fatalf("gitea = %+v, %v", c, err)
	}
	if !strings.Contains(logs.String(), "connector=gitea") {
		t.Errorf("no warning: %s", logs.String())
	}
	r.Fallback = nil
	if _, err := r.New("github", connectors.Config{}); err == nil {
		t.Error("a connector without a factory")
	}

	info := connectors.Info{Capabilities: []connectors.Capability{connectors.CapPullRequests}}
	if !info.Has(connectors.CapPullRequests) || info.Has(connectors.CapAutoComplete) {
		t.Error("Has")
	}
}
