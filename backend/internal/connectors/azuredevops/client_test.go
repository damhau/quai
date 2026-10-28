package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/secrets"
)

// instance is a stand-in for the REST API of an Azure DevOps Server collection, recording the
// calls it gets: enough to check what the client sends, not to test against.
type instance struct {
	*httptest.Server
	maxVersion string
	mu         sync.Mutex
	calls      []call
}

type call struct {
	method, path, version, auth string
}

func newInstance(t *testing.T, maxVersion string) *instance {
	in := &instance{maxVersion: maxVersion}
	in.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in.mu.Lock()
		// The version is in the query, or in the Accept header as the SDK sends it.
		version := r.URL.Query().Get("api-version")
		if _, v, ok := strings.Cut(r.Header.Get("Accept"), "api-version="); ok && version == "" {
			version = v
		}
		in.calls = append(in.calls, call{r.Method, r.URL.EscapedPath(), version, r.Header.Get("Authorization")})
		in.mu.Unlock()
		if r.Header.Get("Authorization") != BasicAuth("pat-1") && r.Header.Get("Authorization") != BasicAuth("pat-2") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodOptions && r.URL.Path == "/tfs/Coll/_apis":
			_ = json.NewEncoder(w).Encode(map[string]any{"count": 1, "value": []map[string]any{{
				"id": gitRepositories.String(), "area": "git", "resourceName": "repositories",
				"routeTemplate": "{project}/_apis/{area}/{resource}/{repositoryId}", "minVersion": "1.0", "maxVersion": in.maxVersion, "releasedVersion": in.maxVersion}}})
		case r.URL.Path == "/tfs/Coll/_apis/connectionData":
			_, _ = w.Write([]byte(`{"deploymentType":"onPremises","authenticatedUser":{"id":"u-1","providerDisplayName":"svc-quai"}}`))
		case r.URL.EscapedPath() == "/tfs/Coll/Infra%20Live/_apis/git/repositories/live":
			_, _ = w.Write([]byte(`{"id":"r-1","name":"live","defaultBranch":"refs/heads/main","webUrl":"https://x/live","project":{"id":"p-1","name":"Infra Live"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"TF401019: The Git repository with name or identifier nope does not exist or you do not have permissions."}`))
		}
	}))
	t.Cleanup(in.Close)
	return in
}

func (in *instance) recorded() []call {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]call(nil), in.calls...)
}

func TestVersionPinning(t *testing.T) {
	ctx := context.Background()
	in := newInstance(t, "7.0")
	pat := "pat-1"
	c, err := New(connectors.Config{URL: in.URL + "/tfs/Coll/Infra%20Live/_git/live",
		Credentials: func(context.Context) (secrets.Secret, bool, error) { return secrets.FromValue(pat), true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ado := c.(*Connector)
	repo, err := ado.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != "r-1" || repo.ProjectID != "p-1" || repo.ProjectName != "Infra Live" {
		t.Errorf("resolved %+v", repo)
	}
	if info := c.Info(); info.Edition != "Server (API 7.0)" || info.APIVersion != "7.0" || info.Platform != "azure-devops" {
		t.Errorf("info = %+v", info)
	}
	in2, _ := ado.Client().Detect(ctx)
	if in2.Services || in2.User.Name != "svc-quai" {
		t.Errorf("instance = %+v", in2)
	}

	// Through the SDK's location service, the version is still the one given, not the SDK's.
	pat = "pat-2" // rotated: the next calls carry it
	var got map[string]any
	if err := ado.Client().Send(ctx, http.MethodGet, gitRepositories, APIVersion, map[string]string{"project": "Infra Live", "repositoryId": "live"}, nil, nil, &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "r-1" {
		t.Errorf("Send = %v", got)
	}
	calls := in.recorded()
	for _, c := range calls {
		if c.method == http.MethodOptions || strings.HasSuffix(c.path, "/connectionData") {
			continue
		}
		if c.version != APIVersion {
			t.Errorf("%s %s was sent with api-version %q", c.method, c.path, c.version)
		}
	}
	if last := calls[len(calls)-1]; last.auth != BasicAuth("pat-2") {
		t.Errorf("the rotated PAT was not used: %s", last.path)
	}

	// Errors: a missing repository, a refused token. Neither shows the token.
	err = ado.Client().Do(ctx, http.MethodGet, path("Infra Live", "_apis", "git", "repositories", "nope"), APIVersion, nil, nil, nil)
	if !errors.Is(err, connectors.ErrNotFound) || !strings.Contains(err.Error(), "TF401019") {
		t.Errorf("a missing repository: %v", err)
	}
	pat = "wrong"
	err = ado.Client().Do(ctx, http.MethodGet, "/_apis/projects", APIVersion, nil, nil, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized || !strings.Contains(err.Error(), "personal access token") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("a refused PAT: %v", err)
	}

	// git authenticates as the API does.
	pat = "pat-1"
	if creds, err := c.GitAuth(ctx); err != nil || creds.Header != "Authorization: "+BasicAuth("pat-1") {
		t.Errorf("GitAuth = %v", err)
	}
}

func TestOldServerRefused(t *testing.T) {
	in := newInstance(t, "6.0")
	c, err := New(connectors.Config{URL: in.URL + "/tfs/Coll/Infra%20Live/_git/live",
		Credentials: func(context.Context) (secrets.Secret, bool, error) { return secrets.FromValue("pat-1"), true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.(*Connector).Resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "Server 2022") {
		t.Errorf("an API 6.0 instance: %v", err)
	}
}
