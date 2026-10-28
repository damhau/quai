package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store"
)

// Managing repository connections (docs/design/01-repository-connections.md#registering-a-repository):
// repository admins create, change and delete those managed through the API, test a connection's
// access, install its webhook, generate its run token and read its health.

// ConnectionView is a stored connection, connected or not.
type ConnectionView struct {
	Name      string                `json:"name"`
	ManagedBy string                `json:"managedBy" enum:"config,api"`
	Document  repositories.Document `json:"document" doc:"The Repository document; secrets appear as references only"`
	Connected bool                  `json:"connected"`
	Error     string                `json:"error,omitempty" doc:"Why the connection could not be opened; it is retried"`
}

// CheckView is one step of the access test.
type CheckView struct {
	Name       string `json:"name" enum:"credentials,connector,read,push,pull-request,status,delete-branch,branch-protection"`
	OK         bool   `json:"ok"`
	Skipped    string `json:"skipped,omitempty" doc:"Why it did not run"`
	Error      string `json:"error,omitempty"`
	Permission string `json:"permission,omitempty" doc:"What the credential needs, when the check failed"`
}

// ScanView is what the default branch holds of Quai's definitions.
type ScanView struct {
	Commit     string        `json:"commit"`
	Found      bool          `json:"found" doc:"The branch has a .quai/ folder; without it, the onboarding assistant proposes types"`
	Types      int           `json:"types"`
	Blueprints int           `json:"blueprints"`
	Problems   []ProblemView `json:"problems"`
}

// AccessView is the access test's outcome.
type AccessView struct {
	OK     bool        `json:"ok"`
	Checks []CheckView `json:"checks"`
	Scan   *ScanView   `json:"scan,omitempty" doc:"Absent when the repository could not be read"`
}

// WebhookView says where the platform delivers the repository's events, and whether Quai
// installed the webhook.
type WebhookView struct {
	URL       string `json:"url"`
	Installed bool   `json:"installed"`
	Manual    string `json:"manual,omitempty" doc:"How to set the webhook up by hand, when Quai could not"`
}

// RunTokenView is a new run token, shown once.
type RunTokenView struct {
	Token  string `json:"token" doc:"For the runner: the pipeline sends it as a bearer token. Shown once; Quai does not keep it"`
	SHA256 string `json:"sha256" doc:"For Quai: store it, in hex, in the secret spec.runner.reportToken.ref names"`
}

// HealthView is a repository's health.
type HealthView struct {
	Connected    bool                   `json:"connected"`
	Error        string                 `json:"error,omitempty"`
	Credentials  CredentialsHealth      `json:"credentials"`
	Sync         SyncHealth             `json:"sync"`
	Enforcement  EnforcementView        `json:"enforcement"`
	Trust        TrustView              `json:"trust"`
	Definitions  DefinitionsHealth      `json:"definitions"`
	PullRequests PullRequestsHealthView `json:"pullRequests"`
}

// CredentialsHealth is whether the connection's credential can be read, and when it expires.
type CredentialsHealth struct {
	Ref         string     `json:"ref,omitempty"`
	Readable    bool       `json:"readable"`
	Error       string     `json:"error,omitempty"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty" doc:"When the secret store says (a Kubernetes Secret's quai.dev/expires-at)"`
	ExpiresSoon bool       `json:"expiresSoon" doc:"Within 30 days"`
}

// SyncHealth is when Quai last heard from the repository.
type SyncHealth struct {
	LastWebhookAt *time.Time `json:"lastWebhookAt,omitempty"`
	LastFetchAt   *time.Time `json:"lastFetchAt,omitempty"`
	LastPollAt    *time.Time `json:"lastPollAt,omitempty"`
	Head          string     `json:"head,omitempty"`
}

// DefinitionsHealth is the definitions at the head of the branch, and their problems by file.
type DefinitionsHealth struct {
	Commit     string        `json:"commit,omitempty"`
	Types      int           `json:"types"`
	Blueprints int           `json:"blueprints"`
	Problems   []ProblemView `json:"problems"`
}

// PullRequestsHealthView counts Quai's pull requests on the repository.
type PullRequestsHealthView struct {
	Open           int        `json:"open" doc:"Waiting for approval or merge"`
	OldestOpenedAt *time.Time `json:"oldestOpenedAt,omitempty"`
	Stale          int        `json:"stale"`
}

type documentInput struct {
	Body repositories.Document
}

type updateRepositoryInput struct {
	Repository string `path:"repository"`
	Body       repositories.Document
}

func registerRepositories(a huma.API, opts Options) {
	reg, s := opts.Repositories, opts.Store
	tags := []string{"Repositories"}

	huma.Register(a, huma.Operation{
		OperationID: "get-repository", Method: http.MethodGet, Path: Prefix + "/repositories/{repository}",
		Summary: "Read a repository connection", Tags: tags,
	}, func(ctx context.Context, in *repositoryInput) (*out[ConnectionView], error) {
		if _, err := authorize(ctx, authz.ManageRepository, authz.Object{Repository: in.Repository}); err != nil {
			return nil, err
		}
		c, err := reg.Connection(ctx, in.Repository)
		if err != nil {
			return nil, repositoryProblem(err)
		}
		return &out[ConnectionView]{Body: connectionView(c)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "create-repository", Method: http.MethodPost, Path: Prefix + "/repositories",
		Summary: "Connect a repository", Tags: tags, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *documentInput) (*out[ConnectionView], error) {
		if _, err := authorize(ctx, authz.ManageRepository, authz.Object{Repository: in.Body.Metadata.Name}); err != nil {
			return nil, err
		}
		c, err := reg.Create(ctx, in.Body)
		if err != nil {
			return nil, repositoryProblem(err)
		}
		return &out[ConnectionView]{Body: connectionView(c)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "update-repository", Method: http.MethodPut, Path: Prefix + "/repositories/{repository}",
		Summary: "Change a repository connection managed through the API", Tags: tags,
	}, func(ctx context.Context, in *updateRepositoryInput) (*out[ConnectionView], error) {
		if _, err := authorize(ctx, authz.ManageRepository, authz.Object{Repository: in.Repository}); err != nil {
			return nil, err
		}
		c, err := reg.Update(ctx, in.Repository, in.Body)
		if err != nil {
			return nil, repositoryProblem(err)
		}
		return &out[ConnectionView]{Body: connectionView(c)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "delete-repository", Method: http.MethodDelete, Path: Prefix + "/repositories/{repository}",
		Summary: "Disconnect a repository managed through the API; its requests and history stay", Tags: tags, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *repositoryInput) (*struct{}, error) {
		if _, err := authorize(ctx, authz.ManageRepository, authz.Object{Repository: in.Repository}); err != nil {
			return nil, err
		}
		if err := reg.Delete(ctx, in.Repository); err != nil {
			return nil, repositoryProblem(err)
		}
		return nil, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "test-repository-access", Method: http.MethodPost, Path: Prefix + "/repository-access-tests",
		Summary: "Test what a connection's credential can do, saved or not", Tags: tags,
		Description: "Reads the repository, pushes a branch under the prefix and deletes it, opens and closes a draft pull request, " +
			"posts a status on it, reads the branch protection, then scans the default branch for definitions.",
	}, func(ctx context.Context, in *documentInput) (*out[AccessView], error) {
		if _, err := authorize(ctx, authz.ManageRepository, authz.Object{Repository: in.Body.Metadata.Name}); err != nil {
			return nil, err
		}
		report, err := reg.AccessTest(ctx, in.Body)
		if err != nil {
			return nil, repositoryProblem(err)
		}
		return &out[AccessView]{Body: accessView(report)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "install-repository-webhook", Method: http.MethodPost, Path: Prefix + "/repositories/{repository}/webhook",
		Summary: "Install the repository's webhook through the platform", Tags: tags,
	}, func(ctx context.Context, in *repositoryInput) (*out[WebhookView], error) {
		if _, err := authorize(ctx, authz.ManageRepository, authz.Object{Repository: in.Repository}); err != nil {
			return nil, err
		}
		repo, err := reg.Get(in.Repository)
		if err != nil {
			return nil, repositoryProblem(err)
		}
		v := WebhookView{URL: webhookURL(opts, repo.Name)}
		if v.URL == "" {
			return nil, huma.Error409Conflict("Quai does not know its public address: set server.publicURL, or webhooks.host")
		}
		err = repo.InstallWebhook(ctx, v.URL)
		switch {
		case err == nil:
			v.Installed = true
		case errors.Is(err, repositories.ErrNoWebhookSecret):
			return nil, huma.Error409Conflict(err.Error())
		default:
			v.Manual = "Quai could not install the webhook (" + err.Error() + "). Add one on the platform that sends push, pull request and status events to " +
				v.URL + ", signed with the secret spec.webhooks.secret.ref names."
		}
		return &out[WebhookView]{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "generate-run-token", Method: http.MethodPost, Path: Prefix + "/repositories/{repository}/run-token",
		Summary: "Generate a run token for the repository's runner", Tags: tags,
		Description: "Returns a new token, once, and its SHA-256. Quai keeps neither: the hash goes in the secret spec.runner.reportToken.ref names, the token in the pipeline.",
	}, func(ctx context.Context, in *repositoryInput) (*out[RunTokenView], error) {
		if _, err := authorize(ctx, authz.ManageRepository, authz.Object{Repository: in.Repository}); err != nil {
			return nil, err
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		token := "quai_run_" + hex.EncodeToString(b)
		sum := sha256.Sum256([]byte(token))
		return &out[RunTokenView]{Body: RunTokenView{Token: token, SHA256: hex.EncodeToString(sum[:])}}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "repository-health", Method: http.MethodGet, Path: Prefix + "/repositories/{repository}/health",
		Summary: "Read a repository's health", Tags: tags,
	}, func(ctx context.Context, in *repositoryInput) (*out[HealthView], error) {
		if _, err := authorize(ctx, authz.ManageRepository, authz.Object{Repository: in.Repository}); err != nil {
			return nil, err
		}
		c, err := reg.Connection(ctx, in.Repository)
		if err != nil {
			return nil, repositoryProblem(err)
		}
		return &out[HealthView]{Body: health(ctx, s, c)}, nil
	})
}

// webhookURL is where the platform delivers the repository's events: on webhooks.host when set,
// else at Quai's public URL; empty when neither is known.
func webhookURL(opts Options, repository string) string {
	switch {
	case opts.WebhookHost != "":
		return "https://" + opts.WebhookHost + "/webhooks/" + repository
	case opts.PublicURL != "":
		return strings.TrimSuffix(opts.PublicURL, "/") + "/webhooks/" + repository
	}
	return ""
}

func connectionView(c repositories.Connection) ConnectionView {
	v := ConnectionView{Name: c.Document.Metadata.Name, ManagedBy: c.ManagedBy, Document: c.Document, Connected: c.Repo != nil}
	if c.Err != nil {
		v.Error = c.Err.Error()
	}
	return v
}

func accessView(r repositories.AccessReport) AccessView {
	v := AccessView{OK: r.OK, Checks: make([]CheckView, len(r.Checks))}
	for i, c := range r.Checks {
		v.Checks[i] = CheckView{Name: c.Name, OK: c.OK, Skipped: c.Skipped, Error: c.Error, Permission: c.Permission}
	}
	if r.Scan != nil {
		v.Scan = &ScanView{Commit: r.Scan.Commit, Found: r.Scan.Found, Types: r.Scan.Types, Blueprints: r.Scan.Blueprints, Problems: []ProblemView{}}
		for _, p := range r.Scan.Problems {
			v.Scan.Problems = append(v.Scan.Problems, ProblemView{Path: p.Path, Field: p.Field, Message: p.Message})
		}
	}
	return v
}

// health gathers a connection's health: what this process knows of it, and what every process
// recorded.
func health(ctx context.Context, s *store.Store, c repositories.Connection) HealthView {
	v := HealthView{Connected: c.Repo != nil, Trust: TrustView{Problems: []string{}}, Enforcement: EnforcementView{Problems: []string{}},
		Definitions: DefinitionsHealth{Problems: []ProblemView{}}, Credentials: CredentialsHealth{Ref: c.Document.Spec.Credentials.Ref}}
	if c.Err != nil {
		v.Error = c.Err.Error()
	}
	if c.Repo == nil {
		return v
	}
	repo := c.Repo
	if secret, ok, err := repo.Credentials(ctx); err != nil {
		v.Credentials.Error = err.Error()
	} else if ok {
		v.Credentials.Readable = true
		if !secret.Expires.IsZero() {
			v.Credentials.ExpiresAt = &secret.Expires
			v.Credentials.ExpiresSoon = secret.ExpiresWithin(repositories.ExpiryWarning, time.Now())
		}
	}
	if snap, err := repo.Snapshot(ctx, false); err == nil {
		rv := repositoryView(repo, snap)
		v.Definitions = DefinitionsHealth{Commit: snap.Commit, Types: rv.Types, Blueprints: rv.Blueprints, Problems: rv.Problems}
	}
	v.Trust, v.Enforcement = trustOf(ctx, s, repo.Name), enforcementOf(ctx, s, repo)
	if s == nil {
		return v
	}
	if row, err := s.GetRepositorySync(ctx, repo.Name); err == nil {
		v.Sync = SyncHealth{LastWebhookAt: timeOf(row.LastWebhookAt.Time, row.LastWebhookAt.Valid), LastFetchAt: timeOf(row.LastFetchAt.Time, row.LastFetchAt.Valid),
			LastPollAt: timeOf(row.LastPollAt.Time, row.LastPollAt.Valid), Head: row.Head}
	}
	if row, err := s.PullRequestHealth(ctx, repo.Name); err == nil {
		v.PullRequests = PullRequestsHealthView{Open: int(row.Open), Stale: int(row.Stale), OldestOpenedAt: timeOf(row.Oldest.Time, row.Oldest.Valid)}
	}
	return v
}

func timeOf(t time.Time, valid bool) *time.Time {
	if !valid {
		return nil
	}
	return &t
}

// repositoryProblem maps the errors of managing connections.
func repositoryProblem(err error) error {
	var invalid *repositories.InvalidError
	switch {
	case errors.As(err, &invalid):
		details := make([]error, len(invalid.Problems))
		for i, p := range invalid.Problems {
			details[i] = &huma.ErrorDetail{Location: "body", Message: p}
		}
		return huma.Error422UnprocessableEntity("the Repository document needs attention", details...)
	case errors.Is(err, repositories.ErrNotFound):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, repositories.ErrExists), errors.Is(err, repositories.ErrReadOnly), errors.Is(err, repositories.ErrNoDatabase):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, connectors.ErrUnsupported):
		return huma.Error409Conflict(err.Error())
	}
	return Problem(err, "body")
}
