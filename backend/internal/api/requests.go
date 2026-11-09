package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// DiffLine is a line of a diff: ' ' kept, '-' removed, '+' added.
type DiffLine struct {
	Op   string `json:"op" enum:" ,-,+"`
	Text string `json:"text"`
}

// ChangeView is a file a request changes.
type ChangeView struct {
	Path string     `json:"path"`
	Op   string     `json:"op" enum:"create,update,delete"`
	Diff []DiffLine `json:"diff"`
}

// RuleView is an approval requirement, with its progress on a request.
type RuleView struct {
	Names     []string `json:"names"`
	Approvers []string `json:"approvers"`
	Min       int      `json:"min"`
	External  string   `json:"external,omitempty"`
	Approvals []string `json:"approvals"`
	// OnBehalfOf names, for an approver of Approvals who approved on someone's behalf through a
	// delegation, that person.
	OnBehalfOf map[string]string `json:"onBehalfOf,omitempty" doc:"For each approver of approvals who approved on someone's behalf, through a delegation: that person"`
	Satisfied  bool              `json:"satisfied"`
	// A rule that reads the plan (docs/design/05#rules-on-the-plan).
	When           string `json:"when,omitempty" doc:"The condition of a rule that reads the plan"`
	WaitingForPlan bool   `json:"waitingForPlan,omitempty" doc:"The rule waits for a plan to say whether it applies"`
	NotApplicable  bool   `json:"notApplicable,omitempty" doc:"The plan says the rule does not apply"`
	PlanError      string `json:"planError,omitempty" doc:"Why the condition could not be evaluated on the plan: the rule applies"`
	NoOneToNotify  bool   `json:"noOneToNotify,omitempty" doc:"The request waits for the rule, and no one who may approve for it is reached by a notification: none signed in to Quai, or Quai knows no verified email of theirs"`
	Overall        bool   `json:"overall,omitempty" doc:"The rule counts different people among the approvers of the request's other rules (separation of duties' requireTwo); its approvers count only when no other rule takes approvals from people"`
}

// PlanView is what a request would change.
type PlanView struct {
	Kind       string       `json:"kind"`
	Action     string       `json:"action,omitempty"`
	Target     string       `json:"target"`
	Repository string       `json:"repository"`
	Type       string       `json:"type,omitempty"`
	Blueprint  string       `json:"blueprint,omitempty"`
	Stack      string       `json:"stack,omitempty"`
	Commit     string       `json:"commit"`
	Hash       string       `json:"hash"`
	Changes    []ChangeView `json:"changes"`
	Rules      []RuleView   `json:"rules"`
	// Codeowners, when a rule's approvers include codeowners.
	Codeowners *CodeownersView `json:"codeowners,omitempty" doc:"When a rule's approvers include codeowners: the owners the Git platform names for the changed paths"`
	Manager    *ManagerView    `json:"manager,omitempty" doc:"When a rule's approvers include manager: the requester's manager, or why there is none"`
	// Unmigrated, for a migrate request: the non-conforming resources the migration leaves as
	// they are, to fix in Git.
	Unmigrated []UnmigratedView `json:"unmigrated,omitempty" doc:"For a migrate request: the non-conforming resources the type's migration does not fix, and why"`
	StepUp     bool             `json:"stepUp,omitempty" doc:"The approvers must have signed in recently (step-up) to review it"`
	Separation *SeparationView  `json:"separation,omitempty" doc:"The repository's separation of duties, as the request would be planned with it"`
}

// SeparationView is the separation of duties a request is planned with
// (docs/design/05-delivery-and-approvals.md#separation-of-duties): the repository's options, and
// with definitionAuthors who changed the request's definitions in that period before it.
type SeparationView struct {
	DefinitionAuthors string `json:"definitionAuthors,omitempty" doc:"The period, such as 7d: who changed the request's definitions in it before the request was planned cannot review it"`
	Destroys          string `json:"destroys,omitempty" enum:"requireTwo" doc:"requireTwo: a plan that destroys something, or no plan yet, needs two different approvers"`
	// Since, From and Commit: the commits that count are those after From up to Commit.
	Since     *time.Time             `json:"since,omitempty" doc:"When the period began"`
	From      string                 `json:"from,omitempty" doc:"The definitions commit Quai had recorded when the period began; absent: it had none, and the whole history counted"`
	Commit    string                 `json:"commit,omitempty" doc:"The request's definitions commit, where the period ends"`
	Authors   []DefinitionAuthorView `json:"authors" doc:"The people who cannot review, each with the commits that name them"`
	Unmatched []string               `json:"unmatched" doc:"The identities of those commits (author, committer, co-authors) that matched no one in Quai's directory by a verified email when the request was planned: whoever signs in with one of their emails, verified, cannot review it either"`
}

// DefinitionAuthorView is a person who changed a request's definitions in the period.
type DefinitionAuthorView struct {
	Name    string   `json:"name"`
	Commits []string `json:"commits"`
}

func separationView(sep requests.Separation) *SeparationView {
	if sep.DefinitionAuthors <= 0 && sep.Destroys == "" {
		return nil
	}
	v := &SeparationView{Destroys: sep.Destroys, From: sep.From, Commit: sep.Commit, Authors: []DefinitionAuthorView{}, Unmatched: []string{}}
	if sep.DefinitionAuthors > 0 {
		v.DefinitionAuthors = sep.DefinitionAuthors.String()
	}
	if !sep.Since.IsZero() {
		v.Since = &sep.Since
	}
	for _, a := range sep.Authors {
		v.Authors = append(v.Authors, DefinitionAuthorView{Name: a.Name, Commits: a.Commits})
	}
	for _, u := range sep.Unmatched {
		v.Unmatched = append(v.Unmatched, u.String())
	}
	return v
}

// CodeownersView is who the Git platform names as owners of a request's paths (CODEOWNERS, or Azure
// DevOps' automatically included reviewers), as Quai matches them
// (docs/design/08-identity-access-tenancy.md#directory).
type CodeownersView struct {
	Owners    []string `json:"owners" doc:"The owners as Quai matches them: user:<name>, group:<name> or email:<address>"`
	Unmatched []string `json:"unmatched" doc:"The owners, as the platform writes them, that match no one: a group without a mapping, a user whose email is unknown"`
}

// ManagerView is the requester's manager, who may approve for a manager rule
// (docs/design/05-delivery-and-approvals.md#approver-kinds): a person, or none and why, and then
// nobody can satisfy the rule.
type ManagerView struct {
	Name   string `json:"name,omitempty" doc:"The manager, a person who signed in to Quai; absent when there is none"`
	Reason string `json:"reason,omitempty" doc:"Why there is no manager: the rule cannot be satisfied"`
}

func managerView(rules []policy.Rule, name, reason string) *ManagerView {
	if !slices.ContainsFunc(rules, func(r policy.Rule) bool { return slices.Contains(r.Approvers, "manager") }) {
		return nil
	}
	return &ManagerView{Name: name, Reason: reason}
}

func codeownersView(rules []policy.Rule, owners, unmatched []string) *CodeownersView {
	if !slices.ContainsFunc(rules, func(r policy.Rule) bool { return slices.Contains(r.Approvers, "codeowners") }) {
		return nil
	}
	if owners == nil {
		owners = []string{}
	}
	if unmatched == nil {
		unmatched = []string{}
	}
	return &CodeownersView{Owners: owners, Unmatched: unmatched}
}

// UnmigratedView is a non-conforming resource a migrate request leaves as it is.
type UnmigratedView struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// ReviewView is a review of a request.
type ReviewView struct {
	Reviewer string    `json:"reviewer"`
	Decision string    `json:"decision"`
	Comment  string    `json:"comment,omitempty"`
	At       time.Time `json:"at"`
	// AuthTime is when the reviewer last authenticated with the identity provider before
	// reviewing; none with an API token.
	AuthTime *time.Time `json:"authTime,omitempty"`
	ACR      string     `json:"acr,omitempty" doc:"The class of the reviewer's authentication, the ID token's acr"`
	StepUp   bool       `json:"stepUp,omitempty" doc:"The reviewer's authentication satisfied the step-up when they reviewed"`
	// OnBehalfOf and Delegation: a review made with the approvals someone delegated.
	OnBehalfOf string `json:"onBehalfOf,omitempty" doc:"The person on whose behalf the reviewer reviewed, with the approvals they delegated: the review counts for them and the reviewer, once"`
	Delegation string `json:"delegation,omitempty" doc:"The delegation the review used"`
	Voided     bool   `json:"voided,omitempty" doc:"The review is void and never counts again: the request expired, or the reviewer (or the person they approved for) was deactivated"`
}

// ReviewForView is someone the caller may review a request for now, with the approvals they
// delegated to the caller (docs/design/05-delivery-and-approvals.md#delegation).
type ReviewForView struct {
	Delegator  string    `json:"delegator"`
	Delegation string    `json:"delegation"`
	Until      time.Time `json:"until" doc:"When the delegation ends"`
}

// EventView is something that happened to a request.
type EventView struct {
	Kind    string          `json:"kind"`
	Actor   string          `json:"actor,omitempty"`
	Data    json.RawMessage `json:"data"`
	At      time.Time       `json:"at"`
	Request string          `json:"request,omitempty" doc:"The request it happened to, on a resource's timeline"`
}

// RequestView is a request.
type RequestView struct {
	ID         string         `json:"id"`
	Number     int64          `json:"number"`
	Kind       string         `json:"kind"`
	Action     string         `json:"action,omitempty"`
	Target     string         `json:"target"`
	Repository string         `json:"repository"`
	Type       string         `json:"type,omitempty"`
	Blueprint  string         `json:"blueprint,omitempty"`
	Requester  string         `json:"requester"`
	Reason     string         `json:"reason"`
	State      string         `json:"state"`
	Stack      string         `json:"stack,omitempty"`
	Inputs     map[string]any `json:"inputs"`
	CreatedAt  time.Time      `json:"createdAt"`
	UpdatedAt  time.Time      `json:"updatedAt"`
	CanReview  bool           `json:"canReview" doc:"You may review it now, in your own right or on someone's behalf"`
	// CanReviewOwn and ReviewFor say in which capacities.
	CanReviewOwn bool            `json:"canReviewOwn,omitempty" doc:"You may review it in your own right"`
	ReviewFor    []ReviewForView `json:"reviewFor,omitempty" doc:"The people whose approvals you hold now, through a delegation, and may review it for"`
	// CannotReview says why the viewer cannot review a request pending approval that a rule
	// names them for.
	CannotReview string            `json:"cannotReview,omitempty" doc:"Why you cannot review this request pending approval although a rule names you: you requested it, or you changed its definitions in the separation of duties' period before it"`
	CanCancel    bool              `json:"canCancel"`
	CanRetry     bool              `json:"canRetry"`
	StepUp       bool              `json:"stepUp" doc:"Reviews count only from people who signed in recently (step-up): the request page asks for it before approve works"`
	Expiry       string            `json:"expiry,omitempty" doc:"The approval expiry its definitions set when it was planned, as written (7d): a request still pending approval then expires"`
	ExpiresAt    *time.Time        `json:"expiresAt,omitempty" doc:"When it expires if it still waits for approval: its submission and its expiry"`
	Rules        []RuleView        `json:"rules"`
	Codeowners   *CodeownersView   `json:"codeowners,omitempty" doc:"When a rule's approvers include codeowners: the owners the Git platform named for the changed paths when the request was submitted"`
	Manager      *ManagerView      `json:"manager,omitempty" doc:"When a rule's approvers include manager: the requester's manager when the request was submitted, or why there was none"`
	Separation   *SeparationView   `json:"separation,omitempty" doc:"The repository's separation of duties when the request was planned: a later change of the options does not change it"`
	Reviews      []ReviewView      `json:"reviews,omitempty"`
	Changes      []ChangeView      `json:"changes,omitempty"`
	Events       []EventView       `json:"events,omitempty"`
	Plans        []RunView         `json:"plans,omitempty" doc:"The latest plan of each stack the request's pull request was planned in"`
	Delivery     *DeliveryView     `json:"delivery,omitempty" doc:"The pull request the change goes through; absent in direct mode"`
	ChangeWindow *ChangeWindowView `json:"changeWindow,omitempty" doc:"What the change windows that apply to it say, while it is pending approval, approved or merging; absent when none applies"`
	// RequesterNotNotified is set when notifications are configured and do not reach the requester.
	RequesterNotNotified bool `json:"requesterNotNotified,omitempty" doc:"Notifications do not reach the requester: Quai knows no verified email of theirs"`
}

// ChangeWindowView is what the change windows say of a request not merged yet, now
// (docs/design/05-delivery-and-approvals.md#change-windows-and-freezes).
type ChangeWindowView struct {
	Open    bool       `json:"open" doc:"Its windows are open: approved, it may merge now"`
	Held    bool       `json:"held" doc:"Approved, Quai holds it until its windows open: quai/approval is pending with the reason, nothing is merged"`
	Opens   *time.Time `json:"opens,omitempty" doc:"When closed: when its windows open next, the earliest it can merge (within a minute of it); absent when none opens within a year, or with error"`
	Closes  *time.Time `json:"closes,omitempty" doc:"When open: when its windows close next; absent when they stay open beyond a year"`
	Zone    string     `json:"zone,omitempty" doc:"The IANA time zone of the window that holds it (or closes first), for its times as the window is written"`
	Windows []string   `json:"windows" doc:"The change windows that apply to it, Quai's configuration's first"`
	Holding []string   `json:"holding,omitempty" doc:"Those closed now; when open, those that close first"`
	Error   string     `json:"error,omitempty" doc:"Why its windows cannot be decided: it waits, whatever the time, until that is fixed"`
	Reason  string     `json:"reason,omitempty" doc:"Approved, what quai/approval says while it waits"`
}

// changeWindowView decides the change windows of a request not merged yet; nil when none applies
// and it is not held.
func changeWindowView(ctx context.Context, svc *requests.Service, r db.Request) *ChangeWindowView {
	if r.State != requests.PendingApproval && r.State != requests.Approved && r.State != requests.Merging {
		return nil
	}
	w, err := svc.WindowsOf(ctx, r)
	if err != nil {
		return nil
	}
	held := r.State == requests.Approved && r.WindowHeldAt.Valid
	if len(w.Applied) == 0 && w.Err == nil && !held {
		return nil
	}
	v := &ChangeWindowView{Open: w.MayMerge(), Held: held, Windows: []string{}}
	for _, x := range w.Applied {
		v.Windows = append(v.Windows, x.Name)
	}
	for _, x := range w.Holding {
		v.Holding = append(v.Holding, x.Name)
	}
	switch {
	case w.Err != nil:
		v.Error = w.Err.Error()
	case w.Open && !w.Closes.IsZero():
		v.Closes = &w.Closes
	case !w.Open && !w.Opens.IsZero():
		v.Opens = &w.Opens
	}
	if len(w.Holding) > 0 {
		v.Zone = w.Zone().String()
	}
	if !v.Open && r.State != requests.PendingApproval {
		v.Reason = w.Reason()
	}
	return v
}

// DeliveryView is a request's pull request.
type DeliveryView struct {
	Branch      string `json:"branch"`
	PullRequest string `json:"pullRequest,omitempty" doc:"Its ID on the platform, once opened"`
	URL         string `json:"url,omitempty"`
	MergeCommit string `json:"mergeCommit,omitempty"`
}

type typeRequestInput struct {
	Body struct {
		requests.TypeInput
		Reason string `json:"reason,omitempty" maxLength:"2000"`
	}
}

type blueprintRequestInput struct {
	Body struct {
		requests.BlueprintInput
		Reason string `json:"reason,omitempty" maxLength:"2000"`
	}
}

type instanceActionRequestInput struct {
	Body struct {
		requests.InstanceActionInput
		Reason string `json:"reason,omitempty" maxLength:"2000"`
	}
}

type listRequestsInput struct {
	View string `query:"view" enum:"mine,inbox,all" default:"mine"`
}

type requestIDInput struct {
	ID string `path:"id" format:"uuid"`
}

type reviewInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Decision string `json:"decision" enum:"approve,deny"`
		Comment  string `json:"comment,omitempty" maxLength:"2000"`
		// Link is the link of a chat card the decision confirms: its checks come on top of the
		// review's.
		Link string `json:"link,omitempty" maxLength:"1024" doc:"The link of the approval card in chat whose decision this confirms"`
		// OnBehalfOf reviews with the approvals someone delegated to the caller.
		OnBehalfOf string `json:"onBehalfOf,omitempty" maxLength:"256" doc:"Review on this person's behalf, with the approvals they delegated to you (reviewFor); absent: in your own right"`
	}
}

// ChatLinkBody is a link of an approval card in chat, to check.
type ChatLinkBody struct {
	Link string `json:"link" minLength:"1" maxLength:"1024"`
}

type chatLinkInput struct {
	ID   string `path:"id" format:"uuid"`
	Body ChatLinkBody
}

// ChatLinkView is what a link of an approval card in chat opens: a decision ready to confirm.
type ChatLinkView struct {
	Decision   string    `json:"decision" enum:"approve,deny"`
	Channel    string    `json:"channel" enum:"teams,slack"`
	ExpiresAt  time.Time `json:"expiresAt"`
	OnBehalfOf string    `json:"onBehalfOf,omitempty" doc:"The decision is on this person's behalf, with the approvals they delegated to you: confirm it with the review's onBehalfOf"`
}

func registerRequests(a huma.API, svc *requests.Service, s *store.Store, log *slog.Logger) {
	planned := func(ctx context.Context, p *requests.Plan, err error) (*requests.Plan, error) {
		if p != nil && p.Authorize != "" {
			for _, o := range append([]authz.Object{p.Object}, p.Also...) {
				if _, err := authorize(ctx, p.Authorize, o); err != nil {
					return nil, err
				}
			}
		}
		if err != nil {
			return nil, Problem(err, "body")
		}
		return p, nil
	}
	who := func(ctx context.Context) auth.Identity { id, _ := auth.FromContext(ctx); return id }

	huma.Register(a, huma.Operation{
		OperationID: "plan-type", Method: http.MethodPost, Path: Prefix + "/plans/type",
		Summary: "Plan a change to a resource", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *struct{ Body requests.TypeInput }) (*out[PlanView], error) {
		if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: in.Body.Repository, CatalogItem: in.Body.Type}); err != nil {
			return nil, err
		}
		p, err := svc.PlanType(ctx, who(ctx), in.Body)
		p, err = planned(ctx, p, err)
		if err != nil {
			return nil, err
		}
		return &out[PlanView]{Body: planView(p)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "plan-blueprint", Method: http.MethodPost, Path: Prefix + "/plans/blueprint",
		Summary: "Plan a new blueprint instance", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *struct{ Body requests.BlueprintInput }) (*out[PlanView], error) {
		if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: in.Body.Repository, CatalogItem: in.Body.Blueprint}); err != nil {
			return nil, err
		}
		p, err := svc.PlanBlueprint(ctx, who(ctx), in.Body)
		p, err = planned(ctx, p, err)
		if err != nil {
			return nil, err
		}
		return &out[PlanView]{Body: planView(p)}, nil
	})

	submit := func(ctx context.Context, p *requests.Plan, reason string) (*out[RequestView], error) {
		r, err := svc.Submit(ctx, who(ctx), p, reason)
		if err != nil {
			return nil, Problem(err, "body")
		}
		return &out[RequestView]{Body: requestView(ctx, svc, s, r, who(ctx), true)}, nil
	}
	huma.Register(a, huma.Operation{
		OperationID: "request-type", Method: http.MethodPost, Path: Prefix + "/requests/type",
		Summary: "Request a change to a resource", Tags: []string{"Requests"}, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *typeRequestInput) (*out[RequestView], error) {
		if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: in.Body.Repository, CatalogItem: in.Body.Type}); err != nil {
			return nil, err
		}
		p, err := svc.PlanType(ctx, who(ctx), in.Body.TypeInput)
		p, err = planned(ctx, p, err)
		if err != nil {
			return nil, err
		}
		return submit(ctx, p, in.Body.Reason)
	})
	huma.Register(a, huma.Operation{
		OperationID: "request-blueprint", Method: http.MethodPost, Path: Prefix + "/requests/blueprint",
		Summary: "Request a new blueprint instance", Tags: []string{"Requests"}, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *blueprintRequestInput) (*out[RequestView], error) {
		if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: in.Body.Repository, CatalogItem: in.Body.Blueprint}); err != nil {
			return nil, err
		}
		p, err := svc.PlanBlueprint(ctx, who(ctx), in.Body.BlueprintInput)
		p, err = planned(ctx, p, err)
		if err != nil {
			return nil, err
		}
		return submit(ctx, p, in.Body.Reason)
	})

	huma.Register(a, huma.Operation{
		OperationID: "plan-instance-action", Method: http.MethodPost, Path: Prefix + "/plans/instance-action",
		Summary: "Plan a day-2 action on a blueprint instance", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *struct{ Body requests.InstanceActionInput }) (*out[PlanView], error) {
		if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: in.Body.Repository, CatalogItem: in.Body.Blueprint}); err != nil {
			return nil, err
		}
		p, err := svc.PlanInstanceAction(ctx, who(ctx), in.Body)
		p, err = planned(ctx, p, err)
		if err != nil {
			return nil, err
		}
		return &out[PlanView]{Body: planView(p)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "request-instance-action", Method: http.MethodPost, Path: Prefix + "/requests/instance-action",
		Summary: "Request a day-2 action on a blueprint instance", Tags: []string{"Requests"}, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *instanceActionRequestInput) (*out[RequestView], error) {
		if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: in.Body.Repository, CatalogItem: in.Body.Blueprint}); err != nil {
			return nil, err
		}
		p, err := svc.PlanInstanceAction(ctx, who(ctx), in.Body.InstanceActionInput)
		p, err = planned(ctx, p, err)
		if err != nil {
			return nil, err
		}
		return submit(ctx, p, in.Body.Reason)
	})

	huma.Register(a, huma.Operation{
		OperationID: "list-requests", Method: http.MethodGet, Path: Prefix + "/requests",
		Summary: "List requests: yours, those waiting for your review, or all you can see", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *listRequestsInput) (*out[[]RequestView], error) {
		me, err := authorize(ctx, authz.ReadRequest, authz.Object{Requester: who(ctx).Principal.Name})
		if err != nil {
			return nil, err
		}
		params := db.ListRequestsParams{States: []string{}, MaxRows: 200}
		switch in.View {
		case "mine":
			params.Requester = me.Principal.Name
		case "inbox":
			params.States = []string{requests.PendingApproval}
		}
		rs, err := s.ListRequests(ctx, params)
		if err != nil {
			return nil, err
		}
		// The approvals the caller holds through delegations: the requests they may review on
		// someone's behalf are theirs to see and wait for them too.
		delegators, err := svc.Delegators(ctx, me.Principal)
		if err != nil {
			return nil, err
		}
		views := []RequestView{}
		for _, r := range rs {
			reviewer := svc.CapacityOf(ctx, r, me.Principal, delegators).Any()
			if in.View == "inbox" && (!reviewer || svc.Reviewed(ctx, r, me.Principal.Name)) {
				continue
			}
			if _, err := authorize(ctx, authz.ReadRequest, requestObject(ctx, svc, r)); err != nil && !reviewer {
				continue
			}
			views = append(views, requestView(ctx, svc, s, r, me, false))
		}
		return &out[[]RequestView]{Body: views}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "get-request", Method: http.MethodGet, Path: Prefix + "/requests/{id}",
		Summary: "Describe a request", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *requestIDInput) (*out[RequestView], error) {
		r, me, err := loadRequest(ctx, svc, in.ID, authz.ReadRequest)
		if err != nil {
			return nil, err
		}
		svc.Refresh(ctx, r)
		return &out[RequestView]{Body: requestView(ctx, svc, s, r, me, true)}, nil
	})
	huma.Register(a, huma.Operation{
		OperationID: "review-request", Method: http.MethodPost, Path: Prefix + "/requests/{id}/review",
		Summary: "Approve or deny a request", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *reviewInput) (*out[RequestView], error) {
		r, me, err := loadRequest(ctx, svc, in.ID, authz.ReviewRequest)
		if err != nil {
			return nil, err
		}
		next, err := svc.ReviewLink(ctx, me, r.ID, requests.ReviewInput{Decision: in.Body.Decision, Comment: in.Body.Comment, Link: in.Body.Link,
			OnBehalfOf: in.Body.OnBehalfOf})
		if err != nil {
			return nil, Problem(err, "body")
		}
		return &out[RequestView]{Body: requestView(ctx, svc, s, next, me, true)}, nil
	})
	huma.Register(a, huma.Operation{
		OperationID: "check-chat-link", Method: http.MethodPost, Path: Prefix + "/requests/{id}/chat-link",
		Summary: "Check the link of an approval card in chat",
		Description: "Says which decision a link of an approval card in chat (Microsoft Teams) opens the request with, " +
			"for the person signed in. It changes nothing: the decision is confirmed with the review, which checks the link again. " +
			"A link is refused (403, type " + ChatLinkProblemType + ") when it was changed, has expired, was sent to someone else, " +
			"or the request was decided or changed since; the refusal is in the audit log.",
		Tags: []string{"Requests"},
	}, func(ctx context.Context, in *chatLinkInput) (*out[ChatLinkView], error) {
		r, me, err := loadRequest(ctx, svc, in.ID, authz.ReadRequest)
		if err != nil {
			return nil, err
		}
		l, err := svc.OpenLink(ctx, me, r, in.Body.Link, "", "")
		if err != nil {
			return nil, Problem(err, "body")
		}
		return &out[ChatLinkView]{Body: ChatLinkView{Decision: l.Decision, Channel: l.Channel, ExpiresAt: l.Expires, OnBehalfOf: l.OnBehalfOf}}, nil
	})
	for _, op := range []struct {
		id, path, summary string
		action            authz.Action
		run               func(context.Context, auth.Identity, uuid.UUID) (db.Request, error)
	}{
		{"cancel-request", "/cancel", "Cancel a request", authz.CancelRequest, svc.Cancel},
		{"retry-request", "/retry", "Deliver a failed request again", authz.RetryRequest, svc.Retry},
	} {
		huma.Register(a, huma.Operation{
			OperationID: op.id, Method: http.MethodPost, Path: Prefix + "/requests/{id}" + op.path,
			Summary: op.summary, Tags: []string{"Requests"},
		}, func(ctx context.Context, in *requestIDInput) (*out[RequestView], error) {
			r, me, err := loadRequest(ctx, svc, in.ID, op.action)
			if err != nil {
				return nil, err
			}
			next, err := op.run(ctx, me, r.ID)
			if err != nil {
				return nil, Problem(err, "path")
			}
			log.InfoContext(ctx, op.id, slog.String("request", r.ID.String()), slog.String("by", me.Principal.Name))
			return &out[RequestView]{Body: requestView(ctx, svc, s, next, me, true)}, nil
		})
	}
}

// loadRequest returns a request if the caller may do action on it. Approvers may read what they
// review even without a role that shows them the request, as may a delegate what they may review
// on the delegator's behalf.
func loadRequest(ctx context.Context, svc *requests.Service, id string, action authz.Action) (db.Request, auth.Identity, error) {
	rid, err := uuid.Parse(id)
	if err != nil {
		return db.Request{}, auth.Identity{}, huma.Error404NotFound("no such request")
	}
	r, err := svc.Get(ctx, rid)
	if err != nil {
		return r, auth.Identity{}, Problem(err, "path")
	}
	me, err := authorize(ctx, action, requestObject(ctx, svc, r))
	if caller, _ := auth.FromContext(ctx); err != nil && action == authz.ReadRequest && svc.MayReview(ctx, r, caller.Principal) {
		return r, caller, nil
	}
	return r, me, err
}

func requestObject(ctx context.Context, svc *requests.Service, r db.Request) authz.Object {
	return authz.Object{Repository: svc.Repository(ctx, r), Requester: r.Requester}
}

func diffView(c plan.Change) []DiffLine {
	lines := []DiffLine{}
	for _, l := range c.Diff() {
		lines = append(lines, DiffLine{Op: string(l.Op), Text: l.Text})
	}
	return lines
}

func ruleViews(rules []policy.Rule, statuses []policy.Status) []RuleView {
	out := []RuleView{}
	for i, r := range rules {
		v := RuleView{Names: r.Names, Approvers: r.Approvers, Min: r.Min, External: r.External, Approvals: []string{},
			WaitingForPlan: r.Undecided(), NotApplicable: r.NotApplicable(), Overall: r.Overall}
		if r.Plan != nil {
			v.When, v.PlanError = r.Plan.When, r.Plan.Error
		}
		if i < len(statuses) {
			v.Approvals, v.OnBehalfOf, v.Satisfied = statuses[i].Approvals, statuses[i].OnBehalfOf, statuses[i].Satisfied
			if v.Approvals == nil {
				v.Approvals = []string{}
			}
		}
		out = append(out, v)
	}
	return out
}

func planView(p *requests.Plan) PlanView {
	v := PlanView{Kind: p.Kind, Action: p.Action, Target: p.Target, Repository: p.Repository, Type: p.Type, Blueprint: p.Blueprint,
		Stack: p.Stack, Commit: p.Commit, Hash: p.ChangeSet.Hash, Changes: []ChangeView{}, Rules: ruleViews(p.Rules, nil),
		Codeowners: codeownersView(p.Rules, p.Codeowners.Entries, p.Codeowners.Unmatched), StepUp: p.StepUp,
		Manager: managerView(p.Rules, p.Manager, p.ManagerReason), Separation: separationView(p.Separation)}
	for _, c := range p.ChangeSet.Changes {
		v.Changes = append(v.Changes, ChangeView{Path: c.Path, Op: c.Op, Diff: diffView(c)})
	}
	for _, u := range p.Unmigrated {
		v.Unmigrated = append(v.Unmigrated, UnmigratedView{Key: u.Key, Reason: u.Reason})
	}
	return v
}

func requestView(ctx context.Context, svc *requests.Service, s *store.Store, r db.Request, me auth.Identity, detail bool) RequestView {
	v := RequestView{ID: r.ID.String(), Number: r.Number, Kind: r.Kind, Action: r.Action, Target: r.Target, Repository: svc.Repository(ctx, r),
		Type: r.TypeName, Blueprint: r.Blueprint, Requester: r.Requester, Reason: r.Reason, State: r.State, Stack: r.Stack,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time, CanReviewOwn: requests.CanReview(r, me.Principal), StepUp: r.StepUp,
		Expiry: r.Expiry}
	if r.ExpiresAt.Valid {
		v.ExpiresAt = &r.ExpiresAt.Time
	}
	if r.State == requests.PendingApproval {
		if delegators, err := svc.Delegators(ctx, me.Principal); err == nil {
			for _, d := range svc.CapacityOf(ctx, r, me.Principal, delegators).For {
				v.ReviewFor = append(v.ReviewFor, ReviewForView{Delegator: d.User.Name, Delegation: d.Delegation.ID.String(), Until: d.Delegation.EndsAt.Time})
			}
		}
	}
	v.CanReview = v.CanReviewOwn || len(v.ReviewFor) > 0
	if !v.CanReviewOwn {
		v.CannotReview = requests.WhyNotReviewer(r, me.Principal)
	}
	v.CanCancel = requests.Cancellable(r.State) && allowed(ctx, authz.CancelRequest, requestObject(ctx, svc, r))
	// A failed apply is not retried: the change is in Git, the next apply reports on it.
	v.CanRetry = r.State == requests.Failed && allowed(ctx, authz.RetryRequest, requestObject(ctx, svc, r)) && !svc.Landed(ctx, r)
	_ = json.Unmarshal(r.Inputs, &v.Inputs)
	var rules []policy.Rule
	_ = json.Unmarshal(r.Rules, &rules)
	reviews, outcome, err := svc.Reviews(ctx, r)
	if err != nil {
		reviews = nil
	}
	// Reviews made void, by its expiry or their reviewer's deactivation, are shown as its history,
	// marked so (they count no longer).
	if voided, err := svc.Voided(ctx, r); err == nil {
		reviews = append(reviews, voided...)
	}
	v.Rules = ruleViews(rules, outcome.Rules)
	v.Codeowners = codeownersView(rules, r.Codeowners, r.CodeownersUnmatched)
	v.Manager = managerView(rules, r.Manager, r.ManagerReason)
	v.Separation = separationView(requests.SeparationOf(r))
	if d, err := s.GetDelivery(ctx, r.ID); err == nil {
		v.Delivery = &DeliveryView{Branch: d.Branch, PullRequest: d.PullRequest, URL: d.Url, MergeCommit: d.MergeCommit}
	}
	if !detail {
		return v
	}
	v.ChangeWindow = changeWindowView(ctx, svc, r)
	// Who notifications do not reach (docs/design/05-delivery-and-approvals.md#notifications).
	if requester, rules, ok, err := svc.Unnotified(ctx, r); err == nil && ok {
		v.RequesterNotNotified = requester
		for _, i := range rules {
			if i < len(v.Rules) {
				v.Rules[i].NoOneToNotify = true
			}
		}
	}
	if runs, err := s.LatestRequestRuns(ctx, db.LatestRequestRunsParams{RequestID: pgtype.UUID{Bytes: r.ID, Valid: true}, Kind: "plan"}); err == nil {
		for _, run := range runs {
			v.Plans = append(v.Plans, runView(run))
		}
	}
	for _, rv := range reviews {
		rview := ReviewView{Reviewer: rv.Reviewer, Decision: rv.Decision, Comment: rv.Comment, At: rv.CreatedAt.Time, ACR: rv.ReviewerAcr, StepUp: rv.StepUp,
			OnBehalfOf: rv.OnBehalfOf, Voided: rv.VoidedAt.Valid}
		if rv.DelegationID.Valid {
			rview.Delegation = uuid.UUID(rv.DelegationID.Bytes).String()
		}
		if rv.ReviewerAuthTime.Valid {
			rview.AuthTime = &rv.ReviewerAuthTime.Time
		}
		v.Reviews = append(v.Reviews, rview)
	}
	if sets, err := s.ListChangeSets(ctx, r.ID); err == nil {
		for _, cs := range sets {
			changes, err := s.ListChanges(ctx, cs.ID)
			if err != nil {
				continue
			}
			for _, c := range changes {
				pc := plan.Change{Path: c.Path, Op: c.Op, Before: c.Before, After: c.After}
				v.Changes = append(v.Changes, ChangeView{Path: c.Path, Op: c.Op, Diff: diffView(pc)})
			}
		}
	}
	if evs, err := s.ListRequestEvents(ctx, pgtype.UUID{Bytes: r.ID, Valid: true}); err == nil {
		for _, e := range evs {
			v.Events = append(v.Events, EventView{Kind: e.Kind, Actor: e.Actor, Data: e.Data, At: e.CreatedAt.Time})
		}
	}
	return v
}
