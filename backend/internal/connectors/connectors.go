// Package connectors is what Quai needs from a Git platform (docs/design/12-git-connectors.md):
// Git credentials, pull requests, statuses, branch protection, ownership rules, webhooks,
// pipelines and identities, behind one interface split into capabilities. A connector reports
// what it supports; the core degrades explicitly where it does not (generic Git has no pull
// requests, so it supports direct delivery only).
//
// Connectors are packages compiled into the binary (connectors/<platform>), made by a Registry
// from a repository connection's spec.connector. Their HTTP calls go through the shared client of
// NewHTTPClient: the CA bundle and proxy of netx, rate limits honoured, nothing secret logged.
package connectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/secrets"
)

// Capability is something a connector may support.
type Capability string

const (
	CapPullRequests   Capability = "pull-requests"
	CapEnforcedStatus Capability = "enforced-status" // a required status only Quai can set
	CapAutoComplete   Capability = "auto-complete"   // the platform merges once policies pass
	CapMergeQueue     Capability = "merge-queue"
	CapSignedWebhooks Capability = "signed-webhooks"
	CapPipelines      Capability = "pipelines"
	CapCodeOwnersFile Capability = "codeowners-file"
)

// Info describes a connector and the platform it reached.
type Info struct {
	Platform     string // the spec.connector name: azure-devops, gitea, generic, …
	Edition      string // such as "Server 2022" or "Services"; empty when it does not matter
	APIVersion   string // the API version calls are pinned to
	Capabilities []Capability
	MergeMethods []MergeMethod // what PullRequests.Merge accepts
}

// Has reports whether the connector supports c.
func (i Info) Has(c Capability) bool { return slices.Contains(i.Capabilities, c) }

// Connector is one platform, for one repository connection. A capability it lacks returns nil.
type Connector interface {
	Info() Info

	// GitAuth returns what git authenticates with: an HTTP header or an SSH key. It is asked
	// for before each command that reaches the remote, so rotated credentials are picked up.
	GitAuth(ctx context.Context) (gitx.Credentials, error)

	PullRequests() PullRequests // open, update, get, merge, auto-complete, abandon
	Statuses() Statuses         // publish quai/approval; list the checks on a pull request or commit
	Policies() Policies         // read branch protection; verify enforcement
	Owners() Owners             // CODEOWNERS or the platform's path-based reviewer rules
	Webhooks() Webhooks         // subscribe, verify, parse into normalized events
	Pipelines() Pipelines       // optional: trigger runs, read their state
	Identities() Identities     // platform users, for mapping to IdP users
}

var (
	// ErrNotFound: the object does not exist, or the credentials cannot see it.
	ErrNotFound = errors.New("not found")
	// ErrBlocked: the platform refuses to merge, because a policy or a required check is not met.
	ErrBlocked = errors.New("blocked by the target branch's policies")
	// ErrUnauthenticated: a webhook delivery that does not carry the secret.
	ErrUnauthenticated = errors.New("the webhook delivery is not authenticated")
	// ErrUnsupported: the platform cannot do this (a merge method it lacks, say).
	ErrUnsupported = errors.New("not supported by the platform")
	// ErrRateLimited: the platform asks to wait longer than Quai waits.
	ErrRateLimited = errors.New("rate limited by the platform")
)

// MergeMethod is how a pull request is merged.
type MergeMethod string

const (
	MergeCommit MergeMethod = "merge"
	Squash      MergeMethod = "squash"
	Rebase      MergeMethod = "rebase"
)

// PullRequestState is where a pull request stands.
type PullRequestState string

const (
	PROpen      PullRequestState = "open"
	PRMerged    PullRequestState = "merged"
	PRAbandoned PullRequestState = "abandoned" // closed without merging
)

// PullRequest is a pull request (a merge request on some platforms).
type PullRequest struct {
	ID           string // the platform's number or ID
	URL          string // its web page
	Title        string
	Description  string
	Source       string // branch names, without refs/heads/
	Target       string
	Head         string // the source's head commit
	State        PullRequestState
	Draft        bool
	Labels       []string
	MergeCommit  string // once merged
	AutoComplete bool   // the platform will merge it once its policies pass
}

// NewPullRequest is a pull request to open.
type NewPullRequest struct {
	Source, Target     string
	Title, Description string
	Labels             []string
	Draft              bool
}

// PullRequestUpdate changes a pull request; nil fields are left as they are.
type PullRequestUpdate struct {
	Title       *string
	Description *string
}

// MergeOptions say how to merge.
type MergeOptions struct {
	// Head is the source commit the approvals cover: the platform refuses to merge if the
	// branch moved since. Empty: whatever the head is.
	Head         string
	Method       MergeMethod
	Message      string // the merge or squash commit's message
	DeleteBranch bool   // delete the source branch afterwards
}

// PullRequests manages pull requests (CapPullRequests).
type PullRequests interface {
	Open(ctx context.Context, pr NewPullRequest) (PullRequest, error)
	Get(ctx context.Context, id string) (PullRequest, error)
	Update(ctx context.Context, id string, u PullRequestUpdate) (PullRequest, error)
	// List returns the open pull requests whose source branch starts with prefix.
	List(ctx context.Context, prefix string) ([]PullRequest, error)
	// Merge merges now; ErrBlocked when the target's policies do not allow it yet.
	Merge(ctx context.Context, id string, o MergeOptions) (PullRequest, error)
	// AutoComplete asks the platform to merge once every policy passes (CapAutoComplete).
	AutoComplete(ctx context.Context, id string, o MergeOptions) error
	Abandon(ctx context.Context, id string) error
}

// StatusState is the state of a status or check.
type StatusState string

const (
	StatusPending       StatusState = "pending"
	StatusSuccess       StatusState = "success"
	StatusFailure       StatusState = "failure"
	StatusError         StatusState = "error"
	StatusNotApplicable StatusState = "not-applicable" // stale: the content it approved changed
)

// Status is a status or check on a commit or pull request, such as quai/approval.
type Status struct {
	Name        string // quai/approval
	State       StatusState
	Description string
	URL         string // a link back to Quai
}

// StatusTarget is what a status is set on: platforms put it on the pull request's latest
// iteration (Azure DevOps) or on its head commit (Gitea, GitHub).
type StatusTarget struct {
	PullRequest string
	Commit      string
}

// Statuses publishes and reads statuses.
type Statuses interface {
	Set(ctx context.Context, t StatusTarget, s Status) error
	List(ctx context.Context, t StatusTarget) ([]Status, error)
}

// Protection is what a branch's protection requires.
type Protection struct {
	Branch         string
	Protected      bool     // pushes and merges are restricted
	RequiredChecks []string // statuses that must succeed before merging
	// ChecksRestricted: only an authorized identity (Quai's) can satisfy RequiredChecks.
	ChecksRestricted bool
	// ResetOnPush: a push to the pull request resets its statuses.
	ResetOnPush bool
	// RequiredApprovals from the platform's reviewers, beyond Quai's.
	RequiredApprovals int
}

// Enforcement is whether a branch really requires a check, and why not.
type Enforcement struct {
	Enforced bool
	Problems []string // what is missing, for the health page
}

// Policies reads branch protection.
type Policies interface {
	Protection(ctx context.Context, branch string) (Protection, error)
	// Verify reports whether branch requires check, as Quai needs it (only Quai can set it
	// where the platform allows restricting it).
	Verify(ctx context.Context, branch, check string) (Enforcement, error)
}

// OwnerRule gives the owners of the paths a pattern matches.
type OwnerRule struct {
	Pattern string   // as the platform writes it: gitignore-style, or a regular expression on Gitea
	Owners  []string // users or groups, as the platform names them
}

// Owners reads ownership rules: a CODEOWNERS file, or path-filtered reviewer policies.
type Owners interface {
	Rules(ctx context.Context, branch string) ([]OwnerRule, error)
}

// Delivery is a webhook delivery, verified and normalized.
type Delivery struct {
	ID     string // the platform's delivery ID, to drop duplicates
	Events []Event
}

// Webhooks installs and parses webhooks. Webhooks are notifications, not truth: Quai re-reads
// what an event names before acting on it.
type Webhooks interface {
	// Subscribe installs, or updates, the webhook delivering this repository's events to url.
	Subscribe(ctx context.Context, url string, secret secrets.Secret) error
	// Parse verifies a delivery with secret (ErrUnauthenticated otherwise) and turns it into
	// events. Events of other repositories, and kinds Quai does not use, are dropped.
	Parse(header http.Header, body []byte, secret secrets.Secret) (Delivery, error)
}

// Run is a pipeline run.
type Run struct {
	Pipeline string
	ID       string
	State    string // queued, running, completed
	Result   string // succeeded, failed, … once completed
	Commit   string
	URL      string
}

// Pipelines triggers and reads pipeline runs (CapPipelines).
type Pipelines interface {
	Trigger(ctx context.Context, pipeline, branch string) (Run, error)
	Get(ctx context.Context, pipeline, id string) (Run, error)
}

// User is a platform user.
type User struct {
	ID    string
	Name  string
	Email string // or UPN
}

// Identities finds platform users, to map them to IdP users.
type Identities interface {
	ByEmail(ctx context.Context, email string) (User, error)
}

// Event is a normalized webhook event: Push, PullRequestChanged, StatusChanged, PolicyChanged or
// PipelineRunChanged. Events belong to the connector's repository.
type Event interface{ event() }

// Push moved a branch.
type Push struct {
	Ref           string // refs/heads/main
	Before, After string
	Paths         []string // the changed paths, when the payload has them
}

// PullRequestAction is what happened to a pull request.
type PullRequestAction string

const (
	PROpened    PullRequestAction = "opened"
	PRUpdated   PullRequestAction = "updated"
	PRCompleted PullRequestAction = "completed"
	PRClosed    PullRequestAction = "abandoned"
)

// PullRequestChanged is a pull request opened, updated, completed or abandoned.
type PullRequestChanged struct {
	ID             string
	Action         PullRequestAction
	Head           string
	Source, Target string
}

// StatusChanged is a status set on a pull request or commit.
type StatusChanged struct {
	PullRequest string
	Commit      string
	Status      Status
}

// PolicyChanged is a branch's protection changed: enforcement is checked again.
type PolicyChanged struct{ Branch string }

// PipelineRunChanged is a pipeline run that changed state.
type PipelineRunChanged struct{ Run Run }

func (Push) event()               {}
func (PullRequestChanged) event() {}
func (StatusChanged) event()      {}
func (PolicyChanged) event()      {}
func (PipelineRunChanged) event() {}

// Config is what a connector is made from: a repository connection's settings and Quai's shared
// clients.
type Config struct {
	URL string // spec.url
	// Credentials resolves spec.credentials.ref; ok is false when the connection names none.
	Credentials func(ctx context.Context) (s secrets.Secret, ok bool, err error)
	KnownHosts  string // spec.credentials.knownHosts: the SSH host keys of the remote
	HTTP        *http.Client
	Log         *slog.Logger
}

// Factory makes a connector.
type Factory func(cfg Config) (Connector, error)

// Registry makes connectors by spec.connector.
type Registry struct {
	Factories map[string]Factory
	// Fallback makes the connectors of platforms whose own connector is not built yet: Git
	// transport only, like generic.
	Fallback func(platform string, cfg Config) (Connector, error)
	HTTP     *http.Client // the shared client (NewHTTPClient); nil: one over the default transport
	Log      *slog.Logger
}

// New makes the connector named name. Config's HTTP and Log default to the registry's.
func (r *Registry) New(name string, cfg Config) (Connector, error) {
	f := r.Factories[name]
	if f == nil {
		if r.Fallback == nil {
			return nil, fmt.Errorf("no %s connector", name)
		}
		if r.Log != nil {
			r.Log.Warn("connector not built yet: Git transport only, as generic", slog.String("connector", name))
		}
		f = func(cfg Config) (Connector, error) { return r.Fallback(name, cfg) }
	}
	if cfg.HTTP == nil {
		if cfg.HTTP = r.HTTP; cfg.HTTP == nil {
			cfg.HTTP = NewHTTPClient(http.DefaultTransport, r.Log)
		}
	}
	if cfg.Log == nil {
		if cfg.Log = r.Log; cfg.Log == nil {
			cfg.Log = slog.New(slog.DiscardHandler)
		}
	}
	if cfg.Credentials == nil {
		cfg.Credentials = func(context.Context) (secrets.Secret, bool, error) { return secrets.Secret{}, false, nil }
	}
	return f(cfg)
}

// Timeout bounds one API call, waits for rate limits included.
const Timeout = 2 * time.Minute
