package github

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	gh "github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/conformance"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/githubtest"
	"github.com/damhau/quai/backend/internal/gitx"
)

// liveConnector returns the connector to a repository of the sandbox, as Quai builds it.
func liveConnector(t *testing.T, s *githubtest.Sandbox, name string, client *http.Client) *Connector {
	t.Helper()
	c, err := New(connectors.Config{URL: s.URL(name), HTTP: client, Credentials: s.PrivateKey, AppID: s.AppID})
	if err != nil {
		t.Fatal(err)
	}
	return c.(*Connector)
}

// liveHarness gives the conformance suite repositories of the sandbox. It offers no enforcement,
// second identity or webhooks yet, so steps 2, 3, 4, 6 and 7 are skipped: they belong to P3-17 and
// P3-18.
type liveHarness struct{ s *githubtest.Sandbox }

func (h liveHarness) Setup(t *testing.T, client *http.Client) *conformance.Repository {
	name, _ := h.s.Repository(t)
	return &conformance.Repository{Connector: liveConnector(t, h.s, name, client), URL: h.s.URL(name), DefaultBranch: githubtest.Branch}
}

// TestLiveConformance runs the suite's repository, branch and pull request parts on the sandbox.
func TestLiveConformance(t *testing.T) {
	conformance.Run(t, liveHarness{githubtest.New(t)})
}

// liveRepo is a repository of the sandbox with the connector to it and a mirror pushing as Quai does.
type liveRepo struct {
	s      *githubtest.Sandbox
	name   string
	c      *Connector
	prs    connectors.PullRequests
	mirror *gitx.Mirror
	n      int
}

func newLiveRepo(t *testing.T, s *githubtest.Sandbox) *liveRepo {
	t.Helper()
	name, _ := s.Repository(t)
	c := liveConnector(t, s, name, s.HTTP)
	return &liveRepo{s: s, name: name, c: c, prs: c.PullRequests(), mirror: liveMirror(t, s, name, c)}
}

func liveMirror(t *testing.T, s *githubtest.Sandbox, name string, c *Connector) *gitx.Mirror {
	t.Helper()
	m, err := gitx.OpenMirror(t.Context(), s.URL(name), filepath.Join(t.TempDir(), "mirror.git"),
		gitx.Options{Committer: gitx.Signature{Name: "Quai", Email: "quai@example.com"}, Auth: c.GitAuth, Proxy: s.Network.Proxy})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// push commits content to path on branch, on top of main.
func (r *liveRepo) push(t *testing.T, branch, path, content string) string {
	t.Helper()
	sha, err := r.mirror.CommitAndPush(t.Context(), gitx.Commit{Branch: branch, Base: githubtest.Branch, Message: "Change " + path + "\n",
		Author:  gitx.Signature{Name: "Bob", Email: "bob@example.com"},
		Changes: []plan.Change{{Op: plan.OpCreate, Path: path, After: []byte(content)}}})
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// open pushes a new branch with a file of its own and opens its pull request, without waiting for
// anything: as delivery does.
func (r *liveRepo) open(t *testing.T, n connectors.NewPullRequest) connectors.PullRequest {
	t.Helper()
	r.n++
	branch := "quai/change-" + strconv.Itoa(r.n)
	head := r.push(t, branch, "change-"+strconv.Itoa(r.n)+".txt", "content\n")
	n.Source, n.Target = branch, githubtest.Branch
	if n.Title == "" {
		n.Title = "Change " + branch
	}
	pr, err := r.prs.Open(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Head != head || pr.Source != branch || pr.State != connectors.PROpen {
		t.Errorf("opened %+v, pushed %s to %s", pr, head, branch)
	}
	return pr
}

// head returns where branch is, or "" when it is gone.
func (r *liveRepo) head(t *testing.T, branch string) string {
	t.Helper()
	ref, resp, err := r.s.Client.Git.GetRef(t.Context(), r.s.Org, r.name, "heads/"+branch)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return ref.GetObject().GetSHA()
}

// waitHead waits for GitHub's pull request to show head, which follows a push by a moment.
func (r *liveRepo) waitHead(t *testing.T, id, head string) {
	t.Helper()
	deadline := time.Now().Add(conformance.Timeout)
	for {
		pr, err := r.prs.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if pr.Head == head {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pull request is at %s, pushed %s", pr.Head, head)
		}
		time.Sleep(time.Second)
	}
}

// TestLiveAuthentication proves the app's authentication: the installation is found from the app's
// key and ID alone, and its token, limited to the one repository, reads and pushes over Git there
// and nowhere else.
func TestLiveAuthentication(t *testing.T) {
	s := githubtest.New(t)
	ctx := t.Context()
	one, other := newLiveRepo(t, s), newLiveRepo(t, s)

	auth, err := one.c.installation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if auth.InstallationID() != s.InstallationID {
		t.Errorf("found the installation %d, the sandbox's is %d", auth.InstallationID(), s.InstallationID)
	}

	// The API: the app is installed on the whole organization, the token reaches one repository.
	all, _, err := s.Client.Apps.ListRepos(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if all.GetTotalCount() < 2 {
		t.Fatalf("the installation reaches %d repositories, so the limit below would prove nothing", all.GetTotalCount())
	}
	scoped, _, err := one.c.client.Apps.ListRepos(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scoped.GetTotalCount() != 1 || len(scoped.Repositories) != 1 || scoped.Repositories[0].GetName() != one.name {
		t.Errorf("the token reaches %d repositories: %+v", scoped.GetTotalCount(), scoped.Repositories)
	}

	// Git: a push, which a token that only read could not do.
	sha := one.push(t, "quai/push", "pushed.txt", "pushed\n")
	if got := one.head(t, "quai/push"); got != sha {
		t.Errorf("quai/push is at %q on GitHub, pushed %s", got, sha)
	}

	// The same token on the other repository: public, so it clones, but the push is refused.
	m := liveMirror(t, s, other.name, one.c)
	_, refused := m.CommitAndPush(ctx, gitx.Commit{Branch: "quai/elsewhere", Base: githubtest.Branch, Message: "Elsewhere\n",
		Author:  gitx.Signature{Name: "Bob", Email: "bob@example.com"},
		Changes: []plan.Change{{Op: plan.OpCreate, Path: "elsewhere.txt", After: []byte("no\n")}}})
	if refused == nil {
		t.Fatal("the token limited to one repository pushed to another")
	}
	if got := other.head(t, "quai/elsewhere"); got != "" {
		t.Errorf("the other repository has quai/elsewhere at %s", got)
	}
	token, err := one.c.token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(refused.Error(), token) {
		t.Error("the refused push's error shows the token")
	}

	// A repository the app is not installed on.
	outside, err := New(connectors.Config{URL: "https://github.com/octocat/Hello-World", HTTP: s.HTTP, Credentials: s.PrivateKey, AppID: s.AppID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = outside.GitAuth(ctx)
	if !errors.Is(err, connectors.ErrNotFound) || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("not installed: %v", err)
	}
}

// TestLiveAuthenticationPrivate reads and pushes on a private repository, where nothing works
// without the token.
func TestLiveAuthenticationPrivate(t *testing.T) {
	s := githubtest.New(t)
	ctx := t.Context()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name := githubtest.Prefix + "private-" + hex.EncodeToString(b)
	if _, _, err := s.Client.Repositories.Create(ctx, s.Org, &gh.Repository{Name: gh.Ptr(name), Private: gh.Ptr(true), AutoInit: gh.Ptr(true)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.Client.Repositories.Delete(context.Background(), s.Org, name); err != nil {
			t.Errorf("delete %s: %v", name, err)
		}
	})
	r := &liveRepo{s: s, name: name}
	deadline := time.Now().Add(time.Minute)
	for r.head(t, githubtest.Branch) == "" {
		if time.Now().After(deadline) {
			t.Fatalf("%s has no %s", name, githubtest.Branch)
		}
		time.Sleep(time.Second)
	}
	r.c = liveConnector(t, s, name, s.HTTP)
	r.prs = r.c.PullRequests()
	r.mirror = liveMirror(t, s, name, r.c)
	pr := r.open(t, connectors.NewPullRequest{})
	merged, err := r.prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: pr.Head, Method: connectors.Squash, Message: "Change\n", DeleteBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.head(t, githubtest.Branch); got != merged.MergeCommit {
		t.Errorf("main is at %s, the merge commit is %s", got, merged.MergeCommit)
	}
}

// TestLivePullRequests covers what the conformance suite does not ask of every connector: GitHub's
// limits, drafts, labels and comments, and what Quai reads back.
func TestLivePullRequests(t *testing.T) {
	s := githubtest.New(t)
	ctx := t.Context()
	r := newLiveRepo(t, s)

	longLabel := strings.Repeat("é", maxLabel+10)
	marker := connectors.Marker("0190b5f0-7f3b-7c3d-9a1e-2f4d6a8c0e12")
	pr := r.open(t, connectors.NewPullRequest{Title: strings.Repeat("t", maxTitle+50), Draft: true, Labels: []string{"quai", longLabel},
		Description: strings.Repeat("é", maxDescription) + "\n\n" + marker})
	got, err := r.prs.Get(ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Draft {
		t.Errorf("not a draft: %+v", got)
	}
	if id, ok := connectors.RequestOf(got.Description); !ok || id != "0190b5f0-7f3b-7c3d-9a1e-2f4d6a8c0e12" {
		t.Errorf("the marker was lost in the cut: %q, %v", id, ok)
	}
	if n := utf8.RuneCountInString(got.Title); n != maxTitle {
		t.Errorf("the title has %d characters", n)
	}
	wantLabel := short(longLabel, maxLabel)
	if len(got.Labels) != 2 || (got.Labels[0] != wantLabel && got.Labels[1] != wantLabel) {
		t.Errorf("labels = %q", got.Labels)
	}

	t.Run("a draft is not merged", func(t *testing.T) {
		_, err := r.prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: pr.Head, Method: connectors.Squash})
		if !errors.Is(err, connectors.ErrBlocked) {
			t.Errorf("merging a draft: %v", err)
		}
	})

	t.Run("update and comment", func(t *testing.T) {
		text := "Described again.\n\n" + marker
		u, err := r.prs.Update(ctx, pr.ID, connectors.PullRequestUpdate{Description: &text})
		if err != nil || u.Description != text {
			t.Errorf("updated %+v, %v", u, err)
		}
		if err := r.prs.Comment(ctx, pr.ID, "Approved by Bob."); err != nil {
			t.Fatal(err)
		}
		comments, _, err := s.Client.Issues.ListComments(ctx, s.Org, r.name, mustNumber(t, pr.ID), nil)
		if err != nil || len(comments) != 1 || comments[0].GetBody() != "Approved by Bob." {
			t.Errorf("comments = %+v, %v", comments, err)
		}
	})

	t.Run("list", func(t *testing.T) {
		found, err := r.prs.List(ctx, pr.Source)
		if err != nil || len(found) != 1 || found[0].ID != pr.ID {
			t.Errorf("listed %+v, %v", found, err)
		}
		if none, err := r.prs.List(ctx, "quai/nothing"); err != nil || len(none) != 0 {
			t.Errorf("listed %+v, %v", none, err)
		}
	})

	t.Run("abandon", func(t *testing.T) {
		if err := r.prs.Abandon(ctx, pr.ID); err != nil {
			t.Fatal(err)
		}
		got, err := r.prs.Get(ctx, pr.ID)
		if err != nil || got.State != connectors.PRAbandoned {
			t.Errorf("abandoned %+v, %v", got, err)
		}
		_, err = r.prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: pr.Head, Method: connectors.Squash})
		if err == nil || errors.Is(err, connectors.ErrBlocked) {
			t.Errorf("merging a closed pull request must fail, not wait: %v", err)
		}
		if found, err := r.prs.List(ctx, pr.Source); err != nil || len(found) != 0 {
			t.Errorf("listed %+v, %v", found, err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		if _, err := r.prs.Get(ctx, "999999"); !errors.Is(err, connectors.ErrNotFound) {
			t.Errorf("get: %v", err)
		}
		if _, err := r.prs.Merge(ctx, "999999", connectors.MergeOptions{Method: connectors.Squash}); !errors.Is(err, connectors.ErrNotFound) {
			t.Errorf("merge: %v", err)
		}
	})
}

// TestLiveMerge runs Merge straight after the push and the opening, with nothing waited for: GitHub
// computes mergeability and may lag. What core reads afterwards (Get's merge commit) must match.
func TestLiveMerge(t *testing.T) {
	s := githubtest.New(t)
	ctx := t.Context()
	r := newLiveRepo(t, s)

	for _, method := range []connectors.MergeMethod{connectors.MergeCommit, connectors.Squash, connectors.Rebase} {
		t.Run(string(method), func(t *testing.T) {
			pr := r.open(t, connectors.NewPullRequest{})
			merged, err := r.prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: pr.Head, Method: method, Message: "Merge it\n\nBody.\n", DeleteBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			if merged.State != connectors.PRMerged || merged.MergeCommit == "" {
				t.Fatalf("merged %+v", merged)
			}
			if got := r.head(t, githubtest.Branch); got != merged.MergeCommit {
				t.Errorf("main is at %s, the merge commit is %s", got, merged.MergeCommit)
			}
			got, err := r.prs.Get(ctx, pr.ID)
			if err != nil || got.State != connectors.PRMerged || got.MergeCommit != merged.MergeCommit {
				t.Errorf("read back %+v, %v, merged %s", got, err, merged.MergeCommit)
			}
			if h := r.head(t, pr.Source); h != "" {
				t.Errorf("the branch is still at %s", h)
			}
			again, err := r.prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: pr.Head, Method: method, DeleteBranch: true})
			if err != nil || again.State != connectors.PRMerged || again.MergeCommit != merged.MergeCommit {
				t.Errorf("merged again: %+v, %v", again, err)
			}
		})
	}

	t.Run("the branch is kept when asked", func(t *testing.T) {
		pr := r.open(t, connectors.NewPullRequest{})
		if _, err := r.prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: pr.Head, Method: connectors.Squash}); err != nil {
			t.Fatal(err)
		}
		if h := r.head(t, pr.Source); h != pr.Head {
			t.Errorf("the branch is at %q", h)
		}
	})

	t.Run("a stale head is refused", func(t *testing.T) {
		pr := r.open(t, connectors.NewPullRequest{})
		head := r.push(t, pr.Source, "moved.txt", "moved\n")
		r.waitHead(t, pr.ID, head) // until GitHub has seen it, a merge of the approved head is a fair one
		_, err := r.prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: pr.Head, Method: connectors.Squash, DeleteBranch: true})
		if !errors.Is(err, connectors.ErrBlocked) {
			t.Fatalf("merging a stale head: %v", err)
		}
		if h := r.head(t, pr.Source); h != head {
			t.Errorf("the branch is at %s, pushed %s", h, head)
		}
		// The approval moving to the new head unblocks it.
		if _, err := r.prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: head, Method: connectors.Squash, DeleteBranch: true}); err != nil {
			t.Errorf("merging the new head: %v", err)
		}
	})

	t.Run("a conflict is blocked", func(t *testing.T) {
		first := r.push(t, "quai/conflict-a", "conflict.txt", "a\n")
		second := r.push(t, "quai/conflict-b", "conflict.txt", "b\n")
		a, err := r.prs.Open(ctx, connectors.NewPullRequest{Source: "quai/conflict-a", Target: githubtest.Branch, Title: "a"})
		if err != nil {
			t.Fatal(err)
		}
		b, err := r.prs.Open(ctx, connectors.NewPullRequest{Source: "quai/conflict-b", Target: githubtest.Branch, Title: "b"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.prs.Merge(ctx, a.ID, connectors.MergeOptions{Head: first, Method: connectors.Squash, DeleteBranch: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.prs.Merge(ctx, b.ID, connectors.MergeOptions{Head: second, Method: connectors.Squash, DeleteBranch: true}); !errors.Is(err, connectors.ErrBlocked) {
			t.Errorf("merging a conflict: %v", err)
		}
	})

	t.Run("a method the repository forbids is unsupported", func(t *testing.T) {
		pr := r.open(t, connectors.NewPullRequest{})
		if _, _, err := s.Client.Repositories.Edit(ctx, s.Org, r.name, &gh.Repository{AllowRebaseMerge: gh.Ptr(false)}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, _, err := s.Client.Repositories.Edit(context.Background(), s.Org, r.name, &gh.Repository{AllowRebaseMerge: gh.Ptr(true)}); err != nil {
				t.Errorf("allow rebase again: %v", err)
			}
		})
		// The setting takes effect shortly after the edit. Until then the merge succeeds, which
		// says nothing, so the test reads the setting back first.
		deadline := time.Now().Add(30 * time.Second)
		for {
			repo, _, err := s.Client.Repositories.Get(ctx, s.Org, r.name)
			if err != nil {
				t.Fatal(err)
			}
			if !repo.GetAllowRebaseMerge() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the repository still allows rebase merges")
			}
			time.Sleep(time.Second)
		}
		_, err := r.prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: pr.Head, Method: connectors.Rebase})
		if !errors.Is(err, connectors.ErrUnsupported) {
			t.Errorf("merging with a forbidden method: %v", err)
		}
	})

	t.Run("two merges at once both land", func(t *testing.T) {
		a, b := r.open(t, connectors.NewPullRequest{}), r.open(t, connectors.NewPullRequest{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, pr := range []connectors.PullRequest{a, b} {
			wg.Go(func() {
				_, errs[i] = r.prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: pr.Head, Method: connectors.Squash, DeleteBranch: true})
			})
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Errorf("merged at once: %v, %v", errs[0], errs[1])
		}
	})
}

// TestLiveForks gives a real fork's pull request a branch named as one of the repository's own. It
// is not taken for Quai's, which core finds by source branch, and merging it does not delete the
// repository's branch of that name.
func TestLiveForks(t *testing.T) {
	s := githubtest.New(t)
	ctx := t.Context()
	r := newLiveRepo(t, s)
	const shared = "quai/shared"

	forkName := r.name + "-fork"
	_, _, err := s.Client.Repositories.CreateFork(ctx, s.Org, r.name, &gh.RepositoryCreateForkOptions{Organization: s.Org, Name: forkName})
	var scheduled *gh.AcceptedError // GitHub makes the fork in the background
	if err != nil && !errors.As(err, &scheduled) {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := s.Client.Repositories.Delete(ctx, s.Org, forkName); err != nil {
			t.Errorf("delete %s: %v", forkName, err)
		}
	})
	var forkMain *gh.Reference
	deadline := time.Now().Add(time.Minute)
	for {
		forkMain, _, err = s.Client.Git.GetRef(ctx, s.Org, forkName, "heads/"+githubtest.Branch)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fork has no %s: %v", githubtest.Branch, err)
		}
		time.Sleep(time.Second)
	}
	if _, _, err := s.Client.Git.CreateRef(ctx, s.Org, forkName, gh.CreateRef{Ref: "refs/heads/" + shared, SHA: forkMain.GetObject().GetSHA()}); err != nil {
		t.Fatal(err)
	}
	forkHead, err := s.AddFiles(ctx, forkName, shared, "From the fork", map[string]string{"fork.txt": "fork\n"})
	if err != nil {
		t.Fatal(err)
	}
	// Both repositories have one owner, so GitHub needs the fork named, as owner/name. The
	// repository's own branch of that name comes after, and the head stays on the fork.
	opened, _, err := s.Client.PullRequests.Create(ctx, s.Org, r.name, &gh.NewPullRequest{Title: gh.Ptr("From a fork"),
		Head: gh.Ptr(s.Org + ":" + shared), HeadRepo: gh.Ptr(s.Org + "/" + forkName), Base: gh.Ptr(githubtest.Branch)})
	if err != nil {
		t.Fatal(err)
	}
	ownHead := r.push(t, shared, "own.txt", "own\n")
	fromFork := strconv.Itoa(opened.GetNumber())

	// Without this, the test would pass having tested nothing.
	read, _, err := s.Client.PullRequests.Get(ctx, s.Org, r.name, opened.GetNumber())
	if err != nil {
		t.Fatal(err)
	}
	if read.GetHead().GetRef() != shared || read.GetHead().GetRepo().GetName() != forkName || read.GetHead().GetSHA() != forkHead {
		t.Fatalf("the pull request's head is %s of %s, at %s", read.GetHead().GetRef(), read.GetHead().GetRepo().GetName(), read.GetHead().GetSHA())
	}

	// What core asks before opening a pull request for its branch: it has none yet, and the fork's
	// must not pass for it.
	found, err := r.prs.List(ctx, shared)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("listed %+v: the fork's pull request %s is not the repository's", found, fromFork)
	}

	merged, err := r.prs.Merge(ctx, fromFork, connectors.MergeOptions{Head: forkHead, Method: connectors.Squash, Message: "From a fork\n", DeleteBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	if merged.State != connectors.PRMerged {
		t.Errorf("merged %+v", merged)
	}
	// Asked twice: a retry finds it merged, and reaches the deletion the other way.
	if _, err := r.prs.Merge(ctx, fromFork, connectors.MergeOptions{Head: forkHead, Method: connectors.Squash, DeleteBranch: true}); err != nil {
		t.Errorf("merged again: %v", err)
	}
	if got := r.head(t, shared); got != ownHead {
		t.Errorf("the repository's own %s is at %q, was %s: merging the fork's pull request touched it", shared, got, ownHead)
	}
	fork := &liveRepo{s: s, name: forkName}
	if got := fork.head(t, shared); got != forkHead {
		t.Errorf("the fork's %s is at %q, was %s", shared, got, forkHead)
	}
}

func mustNumber(t *testing.T, id string) int {
	t.Helper()
	n, err := number(id)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
