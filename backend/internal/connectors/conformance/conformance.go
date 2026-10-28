// Package conformance is the suite every connector passes against a real instance of its
// platform (docs/design/12-git-connectors.md#conformance-suite). A connector's tests call Run
// with a Harness, which creates throwaway repositories on the instance and does there what only
// an administrator can (configuring branch protection, say). Steps the connector's capabilities,
// or the harness, do not cover are skipped, and say why.
package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/secrets"
)

// Check is the status Quai publishes and branch protection requires.
const Check = "quai/approval"

// Harness gives the suite repositories on a live instance of the platform.
type Harness interface {
	// Setup creates a repository with one commit on its default branch, deleted when the test
	// ends, and a connector to it whose API calls go through client.
	Setup(t *testing.T, client *http.Client) *Repository
}

// Repository is a throwaway repository, and what the harness lets the suite do with it.
type Repository struct {
	Connector     connectors.Connector
	URL           string // the Git URL; the connector's GitAuth authenticates to it
	DefaultBranch string

	// RequireCheck makes branch require check before merging, restricted to the connector's
	// identity where the platform allows it (steps 3, 4 and 6; nil skips them).
	RequireCheck func(ctx context.Context, branch, check string) error
	// RemoveEnforcement undoes RequireCheck (step 6).
	RemoveEnforcement func(ctx context.Context, branch string) error
	// Other is a connector to the repository as another identity (step 4; nil skips it).
	Other connectors.Connector
	// AddOwner makes owner (as Owners reports it) the owner of the paths pattern matches
	// (step 6; nil skips the ownership check).
	AddOwner func(ctx context.Context, pattern string) (owner string, err error)
	// WebhooksReachable: the platform can deliver webhooks to a server on this machine (step 7).
	// Otherwise Recorded holds deliveries captured from the platform, and their secret.
	WebhooksReachable bool
	Recorded          []Recorded
}

// Recorded is a webhook delivery captured from the platform.
type Recorded struct {
	Header http.Header
	Body   []byte
	Secret string
}

// Timeout bounds the waits for the platform: a pull request's head following a push, a webhook.
var Timeout = 30 * time.Second

// Run runs the suite.
func Run(t *testing.T, h Harness) {
	ctx := t.Context()
	inject := &injector{base: http.DefaultTransport}
	client := &http.Client{Transport: connectors.NewTransport(inject, slog.New(slog.DiscardHandler)), Timeout: connectors.Timeout}
	repo := h.Setup(t, client)
	c := repo.Connector
	info := c.Info()
	mirror, err := gitx.OpenMirror(ctx, repo.URL, filepath.Join(t.TempDir(), "mirror.git"),
		gitx.Options{Committer: gitx.Signature{Name: "Quai", Email: "quai@example.com"}, Auth: c.GitAuth})
	if err != nil {
		t.Fatal(err)
	}
	s := &suite{repo: repo, mirror: mirror, run: random()}
	prs := c.PullRequests()
	enforced := false

	// 1. Push a branch under quai/, open a PR, update it with a second commit.
	var first connectors.PullRequest
	t.Run("1-pull-request", func(t *testing.T) {
		branch, head := s.push(t, "")
		if prs == nil {
			t.Skip("no pull requests: direct delivery only")
		}
		pr, err := prs.Open(ctx, connectors.NewPullRequest{Source: branch, Target: repo.DefaultBranch,
			Title: "Conformance " + s.run, Description: "Opened by the conformance suite."})
		if err != nil {
			t.Fatal(err)
		}
		if pr.ID == "" || pr.Source != branch || pr.Target != repo.DefaultBranch || pr.State != connectors.PROpen {
			t.Errorf("opened %+v", pr)
		}
		got := s.waitHead(t, pr.ID, head)
		if got.Title != "Conformance "+s.run || got.URL == "" {
			t.Errorf("read back %+v", got)
		}
		_, head2 := s.push(t, branch)
		s.waitHead(t, pr.ID, head2)
		title := "Conformance " + s.run + " (updated)"
		if u, err := prs.Update(ctx, pr.ID, connectors.PullRequestUpdate{Title: &title}); err != nil || u.Title != title {
			t.Errorf("update = %+v, %v", u, err)
		}
		open, err := prs.List(ctx, "quai/")
		if err != nil || !slices.ContainsFunc(open, func(p connectors.PullRequest) bool { return p.ID == pr.ID }) {
			t.Errorf("List(quai/) = %v, %v", open, err)
		}
		first = s.waitHead(t, pr.ID, head2)
	})

	// 2. Publish quai/approval pending, then succeeded, on the latest iteration or commit.
	t.Run("2-status", func(t *testing.T) {
		if first.ID == "" || c.Statuses() == nil {
			t.Skip("no pull request, or no statuses")
		}
		for _, state := range []connectors.StatusState{connectors.StatusPending, connectors.StatusSuccess} {
			s.setStatus(t, c, first, state)
			if got := s.status(t, first); got != state {
				t.Errorf("after setting %s, the platform shows %q", state, got)
			}
		}
	})

	// 3. With enforcement, the PR cannot merge while the status is pending, and can once it
	// succeeded.
	t.Run("3-enforcement", func(t *testing.T) {
		if prs == nil || repo.RequireCheck == nil || len(info.MergeMethods) == 0 {
			t.Skip("no pull requests, merges, or enforcement in the harness")
		}
		if err := repo.RequireCheck(ctx, repo.DefaultBranch, Check); err != nil {
			t.Fatal(err)
		}
		enforced = true
		pr := s.openPR(t)
		s.setStatus(t, c, pr, connectors.StatusPending)
		opts := connectors.MergeOptions{Method: info.MergeMethods[0], Message: "Conformance\n", DeleteBranch: true}
		if _, err := prs.Merge(ctx, pr.ID, opts); !errors.Is(err, connectors.ErrBlocked) {
			t.Errorf("merging with the check pending: %v, want ErrBlocked", err)
		}
		s.setStatus(t, c, pr, connectors.StatusSuccess)
		if merged, err := prs.Merge(ctx, pr.ID, opts); err != nil || merged.State != connectors.PRMerged {
			t.Errorf("merging with the check succeeded: %+v, %v", merged, err)
		}
	})

	// 4. A status published by another identity does not satisfy the policy.
	t.Run("4-status-identity", func(t *testing.T) {
		if !enforced || repo.Other == nil || !info.Has(connectors.CapEnforcedStatus) {
			t.Skip("the platform, or the harness, cannot restrict who sets the check")
		}
		pr := s.openPR(t)
		s.setStatus(t, repo.Other, pr, connectors.StatusSuccess)
		opts := connectors.MergeOptions{Method: info.MergeMethods[0], Message: "Conformance\n", DeleteBranch: true}
		if _, err := prs.Merge(ctx, pr.ID, opts); !errors.Is(err, connectors.ErrBlocked) {
			t.Errorf("merging on another identity's status: %v, want ErrBlocked", err)
		}
		if err := prs.Abandon(ctx, pr.ID); err != nil {
			t.Error(err)
		}
		if got, err := prs.Get(ctx, pr.ID); err != nil || got.State != connectors.PRAbandoned {
			t.Errorf("abandoned: %+v, %v", got, err)
		}
	})

	// 5. Merge with each supported method; check the merge commit and the branch deletion.
	t.Run("5-merge", func(t *testing.T) {
		if prs == nil || len(info.MergeMethods) == 0 {
			t.Skip("no pull requests or merges")
		}
		for _, method := range info.MergeMethods {
			t.Run(string(method), func(t *testing.T) {
				pr := s.openPR(t)
				if enforced {
					s.setStatus(t, c, pr, connectors.StatusSuccess)
				}
				message := "Conformance " + string(method) + "\n\nApproved-by: Conformance <conformance@example.com>\n"
				merged, err := prs.Merge(ctx, pr.ID, connectors.MergeOptions{Method: method, Message: message, DeleteBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				if merged.State != connectors.PRMerged || merged.MergeCommit == "" {
					t.Fatalf("merged %+v", merged)
				}
				s.fetch(t)
				if head, _ := mirror.Head(ctx, repo.DefaultBranch); head != merged.MergeCommit {
					t.Errorf("%s is at %s, the merge commit is %s", repo.DefaultBranch, head, merged.MergeCommit)
				}
				parents, err := mirror.Parents(ctx, merged.MergeCommit)
				if want := map[connectors.MergeMethod]int{connectors.MergeCommit: 2}[method]; err != nil || (want == 2) != (len(parents) == 2) {
					t.Errorf("%s: the merge commit has parents %v, %v", method, parents, err)
				}
				if method != connectors.Rebase {
					if log, err := mirror.Log(ctx, repo.DefaultBranch, "", 1); err != nil || len(log) == 0 || !strings.Contains(log[0].Body, "Approved-by: Conformance") {
						t.Errorf("the merge message lost its trailers: %+v, %v", log, err)
					}
				}
				s.eventually(t, "the source branch is deleted", func() bool {
					s.fetch(t)
					_, err := mirror.Head(ctx, pr.Source)
					return errors.Is(err, gitx.ErrNotFound)
				})
			})
		}
	})

	// 6. Read branch protection and ownership rules; detect when enforcement is removed.
	t.Run("6-policies", func(t *testing.T) {
		pol := c.Policies()
		if pol == nil || !enforced {
			t.Skip("no policies, or no enforcement in the harness")
		}
		p, err := pol.Protection(ctx, repo.DefaultBranch)
		if err != nil || !slices.Contains(p.RequiredChecks, Check) {
			t.Errorf("protection = %+v, %v", p, err)
		}
		if e, err := pol.Verify(ctx, repo.DefaultBranch, Check); err != nil || !e.Enforced {
			t.Errorf("verify = %+v, %v", e, err)
		}
		if repo.AddOwner != nil && c.Owners() != nil {
			owner, err := repo.AddOwner(ctx, "/envs/prod/")
			if err != nil {
				t.Fatal(err)
			}
			rules, err := c.Owners().Rules(ctx, repo.DefaultBranch)
			if err != nil || !slices.ContainsFunc(rules, func(r connectors.OwnerRule) bool { return slices.Contains(r.Owners, owner) }) {
				t.Errorf("owner rules = %+v, %v; want %s", rules, err, owner)
			}
		}
		if repo.RemoveEnforcement == nil {
			return
		}
		if err := repo.RemoveEnforcement(ctx, repo.DefaultBranch); err != nil {
			t.Fatal(err)
		}
		enforced = false
		if e, err := pol.Verify(ctx, repo.DefaultBranch, Check); err != nil || e.Enforced || len(e.Problems) == 0 {
			t.Errorf("after removing it, verify = %+v, %v", e, err)
		}
	})

	// 7. Receive and verify each event type; refuse a delivery with a wrong secret.
	t.Run("7-webhooks", func(t *testing.T) {
		hooks := c.Webhooks()
		if hooks == nil {
			t.Skip("no webhooks")
		}
		var deliveries []Recorded
		switch {
		case repo.WebhooksReachable:
			deliveries = s.receive(t)
		case len(repo.Recorded) > 0:
			deliveries = repo.Recorded
		default:
			t.Skip("the platform cannot reach this machine, and the harness recorded no deliveries")
		}
		kinds := map[string]bool{}
		for _, d := range deliveries {
			got, err := hooks.Parse(d.Header, d.Body, secrets.FromValue(d.Secret))
			if err != nil {
				t.Errorf("a delivery was refused: %v", err)
				continue
			}
			if got.ID == "" {
				t.Error("a delivery without an ID")
			}
			for _, e := range got.Events {
				kinds[kind(e)] = true
			}
			if _, err := hooks.Parse(d.Header, d.Body, secrets.FromValue(d.Secret+"-wrong")); !errors.Is(err, connectors.ErrUnauthenticated) {
				t.Errorf("a delivery with a wrong secret: %v, want ErrUnauthenticated", err)
			}
		}
		for _, k := range []string{"push", "pull-request"} {
			if !kinds[k] {
				t.Errorf("no %s event among %v", k, kinds)
			}
		}
	})

	// 8. Honour Retry-After and back off.
	t.Run("8-rate-limits", func(t *testing.T) {
		var call func() error
		switch {
		case prs != nil && first.ID != "":
			call = func() error { _, err := prs.Get(ctx, first.ID); return err }
		case c.Policies() != nil:
			call = func() error { _, err := c.Policies().Protection(ctx, repo.DefaultBranch); return err }
		default:
			t.Skip("no API calls")
		}
		inject.limit.Store(1)
		start := time.Now()
		if err := call(); err != nil {
			t.Fatal(err)
		}
		if inject.limited.Load() != 1 || time.Since(start) < time.Second {
			t.Errorf("the call did not wait for Retry-After: %d limited, %s", inject.limited.Load(), time.Since(start))
		}
	})
}

type suite struct {
	repo   *Repository
	mirror *gitx.Mirror
	run    string // makes names unique
	n      atomic.Int32
}

// push commits a new file to branch (a new quai/ branch when empty), and returns the branch and
// the commit.
func (s *suite) push(t *testing.T, branch string) (string, string) {
	t.Helper()
	n := s.n.Add(1)
	if branch == "" {
		branch = "quai/conformance-" + s.run + "-" + strconv.Itoa(int(n))
	}
	sha, err := s.mirror.CommitAndPush(t.Context(), gitx.Commit{Branch: branch, Base: s.repo.DefaultBranch,
		Message: "Conformance change\n", Author: gitx.Signature{Name: "Conformance", Email: "conformance@example.com"},
		Changes: []plan.Change{{Op: plan.OpCreate, Path: "conformance/" + s.run + "-" + strconv.Itoa(int(n)) + ".txt", After: []byte(s.run + "\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	return branch, sha
}

// openPR pushes a branch and opens a pull request for it.
func (s *suite) openPR(t *testing.T) connectors.PullRequest {
	t.Helper()
	branch, head := s.push(t, "")
	pr, err := s.repo.Connector.PullRequests().Open(t.Context(), connectors.NewPullRequest{Source: branch, Target: s.repo.DefaultBranch,
		Title: "Conformance " + branch, Description: "Opened by the conformance suite."})
	if err != nil {
		t.Fatal(err)
	}
	return s.waitHead(t, pr.ID, head)
}

// waitHead waits until the platform shows head as the pull request's head.
func (s *suite) waitHead(t *testing.T, id, head string) connectors.PullRequest {
	t.Helper()
	var pr connectors.PullRequest
	s.eventually(t, "the pull request's head is "+head, func() bool {
		var err error
		pr, err = s.repo.Connector.PullRequests().Get(t.Context(), id)
		return err == nil && pr.Head == head
	})
	return pr
}

func (s *suite) setStatus(t *testing.T, c connectors.Connector, pr connectors.PullRequest, state connectors.StatusState) {
	t.Helper()
	if err := c.Statuses().Set(t.Context(), connectors.StatusTarget{PullRequest: pr.ID, Commit: pr.Head},
		connectors.Status{Name: Check, State: state, Description: "Conformance", URL: "https://quai.example.com/requests/1"}); err != nil {
		t.Fatal(err)
	}
}

// status returns the latest state of the check on the pull request, as the platform shows it.
func (s *suite) status(t *testing.T, pr connectors.PullRequest) connectors.StatusState {
	t.Helper()
	list, err := s.repo.Connector.Statuses().List(t.Context(), connectors.StatusTarget{PullRequest: pr.ID, Commit: pr.Head})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range list {
		if st.Name == Check {
			return st.State
		}
	}
	return ""
}

func (s *suite) fetch(t *testing.T) {
	t.Helper()
	if err := s.mirror.Fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func (s *suite) eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(Timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("after %s, still not: %s", Timeout, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// receive subscribes a local server to the repository's webhooks, causes a push, a pull request
// and a status, and returns what the platform delivered.
func (s *suite) receive(t *testing.T) []Recorded {
	t.Helper()
	secret := random()
	var mu sync.Mutex
	var got []Recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<22))
		mu.Lock()
		got = append(got, Recorded{Header: r.Header.Clone(), Body: body, Secret: secret})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	c := s.repo.Connector
	if err := c.Webhooks().Subscribe(t.Context(), srv.URL+"/webhooks/conformance", secrets.FromValue(secret)); err != nil {
		t.Fatal(err)
	}
	if c.PullRequests() != nil {
		pr := s.openPR(t)
		if c.Statuses() != nil {
			s.setStatus(t, c, pr, connectors.StatusPending)
		}
	} else {
		s.push(t, "")
	}
	s.eventually(t, "the platform delivered the webhooks", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 2
	})
	time.Sleep(time.Second) // the stragglers
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(got)
}

func kind(e connectors.Event) string {
	switch e.(type) {
	case connectors.Push:
		return "push"
	case connectors.PullRequestChanged:
		return "pull-request"
	case connectors.StatusChanged:
		return "status"
	case connectors.PolicyChanged:
		return "policy"
	case connectors.PipelineRunChanged:
		return "pipeline"
	}
	return "unknown"
}

func random() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// injector answers the next calls with 429 Too Many Requests and Retry-After: 1, as a throttled
// platform would, while limit is above zero.
type injector struct {
	base    http.RoundTripper
	limit   atomic.Int32
	limited atomic.Int32
}

func (i *injector) RoundTrip(r *http.Request) (*http.Response, error) {
	for n := i.limit.Load(); n > 0; n = i.limit.Load() {
		if i.limit.CompareAndSwap(n, n-1) {
			i.limited.Add(1)
			return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
				Header: http.Header{"Retry-After": {"1"}}, Body: http.NoBody, Request: r}, nil
		}
	}
	return i.base.RoundTrip(r)
}
