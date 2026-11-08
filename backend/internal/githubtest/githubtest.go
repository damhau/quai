// Package githubtest gives live tests the GitHub sandbox (docs/plan/phase-3-backlog.md#test-infrastructure):
// a throwaway organization with a GitHub App installed on it. QUAI_TEST_GITHUB_APP_KEY_FILE names
// the file holding the app's private key; without it, the tests are skipped. QUAI_TEST_GITHUB_ORG,
// QUAI_TEST_GITHUB_APP_ID and QUAI_TEST_GITHUB_INSTALLATION_ID say where it is installed, and
// QUAI_TEST_GITHUB_WEBHOOK_SECRET_FILE names the file holding its webhook's secret. The key and the
// secret are read from their files only and never shown. Tests create what they need with a
// quai-test- prefix and delete it, failed or not.
//
// GitHub cannot reach a test machine: Deliveries reads back, from the app's delivery history, what
// it tried to deliver, to replay it to Quai.
package githubtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
)

const (
	// Prefix starts the name of every repository the tests create.
	Prefix = "quai-test-"
	// Team is the organization's team that tests name in CODEOWNERS.
	Team = "quai-test-owners"
	// Branch is the default branch of the repositories the tests create.
	Branch = "main"
)

// Sandbox is the GitHub organization of the live tests, and the app installed on it.
type Sandbox struct {
	Org            string
	AppID          int64
	InstallationID int64
	Network        *netx.Network  // the environment's proxy, as Quai uses it
	HTTP           *http.Client   // the connectors' shared client over Network, without credentials
	Client         *github.Client // the API as the app's installation on Org
	App            *github.Client // the API as the app itself: its webhook's deliveries

	key     secrets.Secret
	started time.Time
}

// New returns the sandbox, or skips the test. A key without the rest of the sandbox's settings is
// a mistake, not a reason to skip: the run would look green having tested nothing.
func New(t *testing.T) *Sandbox {
	t.Helper()
	file := os.Getenv("QUAI_TEST_GITHUB_APP_KEY_FILE")
	if file == "" {
		t.Skip("QUAI_TEST_GITHUB_APP_KEY_FILE is not set: no live GitHub tests")
	}
	org := os.Getenv("QUAI_TEST_GITHUB_ORG")
	appID, appErr := strconv.ParseInt(os.Getenv("QUAI_TEST_GITHUB_APP_ID"), 10, 64)
	installationID, installationErr := strconv.ParseInt(os.Getenv("QUAI_TEST_GITHUB_INSTALLATION_ID"), 10, 64)
	if org == "" || appErr != nil || installationErr != nil {
		t.Fatal("QUAI_TEST_GITHUB_APP_KEY_FILE needs QUAI_TEST_GITHUB_ORG, QUAI_TEST_GITHUB_APP_ID and QUAI_TEST_GITHUB_INSTALLATION_ID")
	}
	key, err := os.ReadFile(file) // #nosec G304 -- the file the developer named
	if err != nil {
		t.Fatalf("the app's key file: %v", err)
	}
	network, err := netx.New("")
	if err != nil {
		t.Fatal(err)
	}
	apps, err := ghinstallation.NewAppsTransport(connectors.NewTransport(network.Transport(), nil), appID, key)
	if err != nil {
		t.Fatalf("the app's key: %v", err)
	}
	client := func(auth http.RoundTripper) *github.Client {
		c, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: auth, Timeout: connectors.Timeout}))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return &Sandbox{
		Org: org, AppID: appID, InstallationID: installationID, Network: network,
		HTTP:    connectors.NewHTTPClient(network.Transport(), nil),
		Client:  client(ghinstallation.NewFromAppsTransport(apps, installationID)),
		App:     client(apps),
		key:     secrets.FromValue(string(key)),
		started: time.Now(),
	}
}

// PrivateKey resolves the app's private key, as a connector's Credentials does.
func (s *Sandbox) PrivateKey(context.Context) (secrets.Secret, bool, error) { return s.key, true, nil }

// Get is the private key as a secret backend: a reference of any name resolves to it.
func (s *Sandbox) Get(context.Context, string) (secrets.Secret, error) { return s.key, nil }

// WebhookSecret returns the secret GitHub signs the app's deliveries with, or skips the test.
func (s *Sandbox) WebhookSecret(t *testing.T) secrets.Secret {
	t.Helper()
	file := os.Getenv("QUAI_TEST_GITHUB_WEBHOOK_SECRET_FILE")
	if file == "" {
		t.Skip("QUAI_TEST_GITHUB_WEBHOOK_SECRET_FILE is not set: no live GitHub webhook tests")
	}
	b, err := os.ReadFile(file) // #nosec G304 -- the file the developer named
	if err != nil {
		t.Fatalf("the webhook secret's file: %v", err)
	}
	return secrets.FromValue(strings.TrimSpace(string(b)))
}

// Repository creates a public repository with a first commit on main, and deletes it when the
// test ends. Its name starts with quai-test-. It is public because merge queues need it, so it
// holds synthetic data only.
func (s *Sandbox) Repository(t *testing.T) (name string, id int64) {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name = Prefix + hex.EncodeToString(b)
	repo, _, err := s.Client.Repositories.Create(ctx, s.Org, &github.Repository{Name: github.Ptr(name), Private: github.Ptr(false), AutoInit: github.Ptr(true)})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := s.Client.Repositories.Delete(ctx, s.Org, name); err != nil {
			t.Errorf("delete %s: %v", name, err)
		}
	})
	// The first commit shows up shortly after the repository does.
	deadline := time.Now().Add(time.Minute)
	for {
		_, _, err := s.Client.Git.GetRef(ctx, s.Org, name, "heads/"+Branch)
		if err == nil {
			return name, repo.GetID()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s has no %s: %v", name, Branch, err)
		}
		time.Sleep(time.Second)
	}
}

// URL returns a repository's Git URL.
func (s *Sandbox) URL(name string) string {
	return "https://github.com/" + s.Org + "/" + name
}

// AddFiles commits files (path → content), in one commit, on top of branch. It returns the commit.
func (s *Sandbox) AddFiles(ctx context.Context, repo, branch, message string, files map[string]string) (string, error) {
	ref, _, err := s.Client.Git.GetRef(ctx, s.Org, repo, "heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("branch %s: %w", branch, err)
	}
	parent := ref.GetObject().GetSHA()
	entries := make([]*github.TreeEntry, 0, len(files))
	for path, content := range files {
		entries = append(entries, &github.TreeEntry{Path: github.Ptr(strings.TrimPrefix(path, "/")), Mode: github.Ptr("100644"), Type: github.Ptr("blob"), Content: github.Ptr(content)})
	}
	tree, _, err := s.Client.Git.CreateTree(ctx, s.Org, repo, parent, entries)
	if err != nil {
		return "", fmt.Errorf("the tree: %w", err)
	}
	commit, _, err := s.Client.Git.CreateCommit(ctx, s.Org, repo, github.Commit{Message: github.Ptr(message), Tree: tree, Parents: []*github.Commit{{SHA: github.Ptr(parent)}}}, nil)
	if err != nil {
		return "", fmt.Errorf("the commit: %w", err)
	}
	if _, _, err := s.Client.Git.UpdateRef(ctx, s.Org, repo, "heads/"+branch, github.UpdateRef{SHA: commit.GetSHA()}); err != nil {
		return "", fmt.Errorf("branch %s: %w", branch, err)
	}
	return commit.GetSHA(), nil
}

// ApprovalRuleset is a ruleset on the default branch requiring check from the app, which nobody
// bypasses: the enforcement Quai needs. Its bypass list is empty rather than left out, which an
// update would take as "unchanged".
func (s *Sandbox) ApprovalRuleset(check string) github.RepositoryRuleset {
	return github.RepositoryRuleset{Name: "quai", Target: github.Ptr(github.RulesetTargetBranch), Enforcement: github.RulesetEnforcementActive,
		BypassActors: []*github.BypassActor{},
		Conditions: &github.RepositoryRulesetConditions{RefName: &github.RepositoryRulesetRefConditionParameters{Include: []string{"~DEFAULT_BRANCH"}, Exclude: []string{}}},
		Rules: &github.RepositoryRulesetRules{RequiredStatusChecks: &github.RequiredStatusChecksRuleParameters{
			RequiredStatusChecks: []*github.RuleStatusCheck{{Context: check, IntegrationID: github.Ptr(s.AppID)}}}}}
}

// RequireCheck creates ApprovalRuleset on repo, and returns its ID.
func (s *Sandbox) RequireCheck(ctx context.Context, repo, check string) (int64, error) {
	set, _, err := s.Client.Repositories.CreateRuleset(ctx, s.Org, repo, s.ApprovalRuleset(check))
	if err != nil {
		return 0, fmt.Errorf("the ruleset: %w", err)
	}
	return set.GetID(), nil
}

// AddTeam gives Team write access to repo, which GitHub needs of a team that owns files.
func (s *Sandbox) AddTeam(ctx context.Context, repo string) error {
	if _, err := s.Client.Teams.AddTeamRepoBySlug(ctx, s.Org, Team, s.Org, repo, &github.TeamAddTeamRepoOptions{Permission: "push"}); err != nil {
		return fmt.Errorf("team %s on %s: %w", Team, repo, err)
	}
	return nil
}

// Delivery is a request GitHub tried to send, as the app's delivery history shows it.
type Delivery struct {
	ID     int64
	Event  string // the X-GitHub-Event header: push, pull_request, …
	Action string // the payload's action, when it has one: opened, synchronize, …
	Header http.Header
	Body   []byte // the JSON payload
}

// Deliveries waits until GitHub recorded deliveries for a repository that enough accepts, and
// returns them, oldest first, without redeliveries.
func (s *Sandbox) Deliveries(t *testing.T, repositoryID int64, enough func([]Delivery) bool) []Delivery {
	t.Helper()
	return s.DeliveriesWithin(t, 2*time.Minute, repositoryID, enough)
}

// DeliveriesWithin is Deliveries, waiting up to timeout.
func (s *Sandbox) DeliveriesWithin(t *testing.T, timeout time.Duration, repositoryID int64, enough func([]Delivery) bool) []Delivery {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	seen := map[int64]Delivery{} // the history is append-only: a delivery is read once
	for {
		if err := s.readDeliveries(ctx, repositoryID, seen); err != nil {
			t.Fatal(err)
		}
		out := make([]Delivery, 0, len(seen))
		for _, d := range seen {
			out = append(out, d)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		if enough(out) {
			return out
		}
		if time.Now().After(deadline) {
			byEvent := map[string]int{}
			for _, d := range out {
				byEvent[d.Event]++
			}
			t.Fatalf("GitHub did not record the deliveries in time: %d so far, %v", len(out), byEvent)
		}
		time.Sleep(5 * time.Second)
	}
}

// readDeliveries adds a repository's deliveries that are not in seen yet. The history is the whole
// app's, newest first: it stops at what is older than this sandbox, give or take the clocks.
func (s *Sandbox) readDeliveries(ctx context.Context, repositoryID int64, seen map[int64]Delivery) error {
	since := s.started.Add(-5 * time.Minute)
	opts := &github.ListCursorOptions{PerPage: 100}
	for {
		list, resp, err := s.App.Apps.ListHookDeliveries(ctx, opts)
		if err != nil {
			return fmt.Errorf("the app's deliveries: %w", err)
		}
		for _, d := range list {
			if d.GetDeliveredAt().Before(since) {
				return nil
			}
			if _, ok := seen[d.GetID()]; ok || d.GetRepositoryID() != repositoryID || d.GetRedelivery() {
				continue
			}
			one, _, err := s.App.Apps.GetHookDelivery(ctx, d.GetID())
			if err != nil {
				return fmt.Errorf("delivery %d: %w", d.GetID(), err)
			}
			seen[d.GetID()] = newDelivery(one)
		}
		if resp.Cursor == "" {
			return nil
		}
		opts.Cursor = resp.Cursor
	}
}

func newDelivery(d *github.HookDelivery) Delivery {
	h := http.Header{}
	for k, v := range d.GetRequest().Headers {
		h.Set(k, v)
	}
	var body []byte
	if p := d.GetRequest().RawPayload; p != nil {
		body = *p
	}
	return Delivery{ID: d.GetID(), Event: d.GetEvent(), Action: d.GetAction(), Header: h, Body: body}
}

// HasEvent reports whether deliveries hold one of an event, of any action when action is empty.
func HasEvent(deliveries []Delivery, event, action string) bool {
	for _, d := range deliveries {
		if d.Event == event && (action == "" || d.Action == action) {
			return true
		}
	}
	return false
}
