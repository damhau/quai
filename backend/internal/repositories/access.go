package repositories

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/gitx"
)

// The access test of registration (docs/design/01-repository-connections.md#registering-a-repository):
// with a connection's document, before or after it is saved, Quai checks that its credential can
// do what Quai does, each check with the permission it needs when it fails, then scans the
// default branch for definitions. It leaves nothing behind: the test branch and its pull request
// are removed.

// The checks, in order.
const (
	CheckCredentials = "credentials"
	CheckConnector   = "connector"
	CheckRead        = "read"
	CheckPush        = "push"
	CheckPullRequest = "pull-request"
	CheckStatus      = "status"
	CheckDelete      = "delete-branch"
	CheckProtection  = "branch-protection"
)

// Check is one step of the access test.
type Check struct {
	Name       string
	OK         bool
	Skipped    string // why it was not run: an earlier check failed, or the platform has no such thing
	Error      string
	Permission string // what the credential needs for it, when it failed
}

// Scan is what the default branch holds of Quai's definitions.
type Scan struct {
	Commit     string
	Found      bool // a .quai/ folder
	Types      int
	Blueprints int
	Problems   definitions.Problems
}

// AccessReport is the access test's outcome.
type AccessReport struct {
	OK     bool // every check that ran passed
	Checks []Check
	Scan   *Scan // nil when the repository could not be read
}

// permissions says, by platform and check, what a credential needs.
var permissions = map[string]map[string]string{
	"azure-devops": { // #nosec G101 -- the permissions a credential needs, not credentials
		CheckCredentials: "a PAT at spec.credentials.ref that Quai can read",
		CheckConnector:   "an Azure DevOps repository URL (https://dev.azure.com/<org>/<project>/_git/<repository>, or a Server collection's)",
		CheckRead:        "Read on the repository",
		CheckPush:        "Create branch and Contribute on the branch folder refs/heads/%s*",
		CheckPullRequest: "Contribute to pull requests on the repository",
		CheckStatus:      "Contribute to pull requests on the repository (statuses are posted on pull requests)",
		CheckDelete:      "Force push on the branch folder refs/heads/%s* (deleting a branch)",
		CheckProtection:  "View project-level information, to read the branch policies",
	},
	"gitea": { // #nosec G101 -- the permissions a credential needs, not credentials
		CheckCredentials: "a token at spec.credentials.ref that Quai can read",
		CheckConnector:   "a Gitea repository URL (https://<host>/<owner>/<repository>.git)",
		CheckRead:        "read access to the repository",
		CheckPush:        "write access to the repository, and no protection on the branches under %s",
		CheckPullRequest: "write access to the repository, and a token with the repository and issue scopes (Gitea labels pull requests as issues)",
		CheckStatus:      "write access to the repository",
		CheckDelete:      "write access to the repository, and no protection on the branches under %s",
		CheckProtection:  "admin access to the repository: Gitea shows branch protection to admins",
	},
	"generic": { // #nosec G101 -- the permissions a credential needs, not credentials
		CheckCredentials: "an SSH deploy key at spec.credentials.ref that Quai can read",
		CheckRead:        "read access for the deploy key, and the server's host keys in spec.credentials.knownHosts",
		CheckPush:        "write access for the deploy key",
		CheckDelete:      "write access for the deploy key",
	},
}

// accessRun records the checks as they run.
type accessRun struct {
	report   AccessReport
	platform string
	prefix   string
	failed   bool
}

func (a *accessRun) check(name string, err error) bool {
	c := Check{Name: name, OK: err == nil}
	if err != nil {
		c.Error = err.Error()
		if p := permissions[a.platform][name]; p != "" {
			if name == CheckPush || name == CheckDelete {
				p = fmt.Sprintf(p, a.prefix)
			}
			c.Permission = p
		}
		a.failed = true
	}
	a.report.Checks = append(a.report.Checks, c)
	return err == nil
}

func (a *accessRun) skip(name, why string) {
	a.report.Checks = append(a.report.Checks, Check{Name: name, Skipped: why})
}

// skipRest skips the checks not run yet after the one that failed.
func (a *accessRun) skipRest(after string) {
	order := []string{CheckCredentials, CheckConnector, CheckRead, CheckPush, CheckPullRequest, CheckStatus, CheckDelete, CheckProtection}
	done := map[string]bool{}
	for _, c := range a.report.Checks {
		done[c.Name] = true
	}
	for _, name := range order {
		if !done[name] {
			a.skip(name, "the "+after+" check failed")
		}
	}
}

// AccessTest runs the access test with document d, saved or not.
func (r *Registry) AccessTest(ctx context.Context, d Document) (AccessReport, error) {
	d.ApplyDefaults()
	if err := d.Validate(); err != nil {
		return AccessReport{}, err
	}
	spec := d.Spec
	a := &accessRun{platform: spec.Connector, prefix: spec.Delivery.BranchPrefix}
	repo := &Repo{Name: d.Metadata.Name, Branch: spec.DefaultBranch, Document: d, secrets: r.secrets, log: r.log}
	if spec.Credentials.Ref == "" {
		a.skip(CheckCredentials, "the connection names no credentials")
	} else if _, _, err := repo.Credentials(ctx); !a.check(CheckCredentials, err) {
		a.skipRest(CheckCredentials)
		return a.done(), nil
	}
	conn, err := r.connectors.New(spec.Connector, connectors.Config{URL: spec.URL, Credentials: repo.Credentials, KnownHosts: spec.Credentials.KnownHosts})
	if !a.check(CheckConnector, err) {
		a.skipRest(CheckConnector)
		return a.done(), nil
	}
	if err := os.MkdirAll(r.cache, 0o750); err != nil {
		return AccessReport{}, err
	}
	dir, err := os.MkdirTemp(r.cache, "access-test-*")
	if err != nil {
		return AccessReport{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	m, err := gitx.OpenMirror(ctx, spec.URL, filepath.Join(dir, "mirror.git"), gitx.Options{Committer: r.committer, Auth: conn.GitAuth, CAFile: r.caFile,
		Proxy: r.proxy, SigningKey: r.signing})
	var head string
	if err == nil {
		if head, err = m.Head(ctx, spec.DefaultBranch); errors.Is(err, gitx.ErrNotFound) {
			err = fmt.Errorf("the repository has no branch %s", spec.DefaultBranch)
		}
	}
	if !a.check(CheckRead, err) {
		a.skipRest(CheckRead)
		return a.done(), nil
	}
	a.push(ctx, d, conn, m, r.committer)
	if pol := conn.Policies(); pol == nil {
		a.skip(CheckProtection, "the "+spec.Connector+" connector has no branch protection")
	} else {
		_, err := pol.Protection(ctx, spec.DefaultBranch)
		a.check(CheckProtection, err)
	}
	if a.report.Scan, err = scan(ctx, m, head); err != nil {
		return AccessReport{}, err
	}
	return a.done(), nil
}

func (a *accessRun) done() AccessReport {
	a.report.OK = !a.failed
	return a.report
}

// push pushes a test branch, opens a draft pull request for it, posts a status on it, then
// closes the pull request and deletes the branch.
func (a *accessRun) push(ctx context.Context, d Document, conn connectors.Connector, m *gitx.Mirror, author gitx.Signature) {
	spec := d.Spec
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	branch := spec.Delivery.BranchPrefix + "access-test-" + hex.EncodeToString(b)
	sha, err := m.CommitAndPush(ctx, gitx.Commit{Branch: branch, Base: spec.DefaultBranch, Author: author,
		Message: "Quai's access test\n\nQuai checks that it can push a branch; it deletes it at once.\n",
		Changes: []plan.Change{{Op: plan.OpCreate, Path: ".quai-access-test", After: []byte("Quai's access test: this branch is deleted at once.\n")}}})
	if !a.check(CheckPush, err) {
		for _, name := range []string{CheckPullRequest, CheckStatus, CheckDelete} {
			a.skip(name, "the push check failed")
		}
		return
	}
	if prs := conn.PullRequests(); prs == nil {
		a.skip(CheckPullRequest, "the "+spec.Connector+" connector has no pull requests")
		a.skip(CheckStatus, "the "+spec.Connector+" connector has no pull requests")
	} else {
		// Labelled as Quai's are, which takes more permissions on some platforms.
		pr, err := prs.Open(ctx, connectors.NewPullRequest{Source: branch, Target: spec.DefaultBranch, Draft: true, Title: "Quai's access test", Labels: []string{"quai"},
			Description: "Quai checks that it can open a pull request and post a status on it; it closes it at once."})
		if a.check(CheckPullRequest, err) {
			err := conn.Statuses().Set(ctx, connectors.StatusTarget{PullRequest: pr.ID, Commit: sha},
				connectors.Status{Name: "quai/access-test", State: connectors.StatusSuccess, Description: "Quai's access test"})
			a.check(CheckStatus, err)
			if err := prs.Abandon(ctx, pr.ID); err != nil {
				a.report.Checks[len(a.report.Checks)-2].Error = "opened, but could not be closed: " + err.Error()
				a.report.Checks[len(a.report.Checks)-2].OK = false
				a.failed = true
			}
		} else {
			a.skip(CheckStatus, "the pull-request check failed")
		}
	}
	a.check(CheckDelete, m.DeleteBranch(ctx, branch))
}

// scan reads the definitions at commit.
func scan(ctx context.Context, m *gitx.Mirror, commit string) (*Scan, error) {
	files, err := m.Files(ctx, commit)
	if err != nil {
		return nil, err
	}
	s := &Scan{Commit: commit}
	if info, err := fs.Stat(files, ".quai"); err == nil && info.IsDir() {
		s.Found = true
	}
	defs, problems := definitions.Load(files)
	s.Types, s.Blueprints, s.Problems = len(defs.Types), len(defs.Blueprints), problems
	return s, nil
}
