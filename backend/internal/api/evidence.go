package api

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/evidence"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Evidence bundles (docs/design/09-security-and-compliance.md#evidence-bundle): a request's, for
// auditors, platform admins and the request's own people, and a sample's, for auditors, in one
// archive.

const (
	// maxSample bounds the requests a sample lists, maxPeriod its period.
	maxSample = 500
	maxPeriod = 366 * 24 * time.Hour
	// maxArchive bounds the requests an archive holds, maxArchiveBytes its files' bytes.
	maxArchive      = 50
	maxArchiveBytes = 256 << 20
)

// archiveSlots bounds the archives made at once: each holds a request's bundle in memory at a time.
var archiveSlots = make(chan struct{}, 2)

// Why a bundle carries no proof.
const (
	proofForToken = "a bundle downloaded with an API token carries no proof: the segments of the audit log that prove its events hold " +
		"other requests' events, which an auditor of all repositories or a platform admin downloads signed in; " +
		"the full log proves its events too (quai audit export, quai audit verify)"
	proofForAuditors = "the bundle carries no proof: the segments of the audit log that prove its events hold other requests' events, " +
		"which only auditors of all repositories and platform admins read; the full log proves its events (quai audit export, quai audit verify)"
)

type evidenceInput struct {
	ID     string `path:"id" doc:"The request's ID"`
	Format string `query:"format" enum:"json,pdf" default:"json" doc:"The JSON bundle, the evidence, or its PDF summary"`
}

type sampleInput struct {
	Since      time.Time `query:"since" required:"true" doc:"Requests created at or after this time"`
	Until      time.Time `query:"until" required:"true" doc:"Requests created before this time: at most 366 days after since"`
	Repository string    `query:"repository" doc:"Only the requests of this repository"`
	State      string    `query:"state" enum:"pending_approval,approved,rejected,cancelled,stale,merging,applying,applied,failed,expired" doc:"Only the requests in this state"`
	BreakGlass bool      `query:"breakGlass" doc:"Only the requests merged by break-glass"`
	Limit      int       `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

// EvidenceRequestView is a request of an audit sample.
type EvidenceRequestView struct {
	ID         string    `json:"id"`
	Number     int64     `json:"number"`
	Repository string    `json:"repository"`
	Target     string    `json:"target"`
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
	Requester  string    `json:"requester"`
	BreakGlass bool      `json:"breakGlass"`
	CreatedAt  time.Time `json:"createdAt"`
}

type sampleOutput struct {
	Body struct {
		Requests []EvidenceRequestView `json:"requests"`
		// Truncated: more requests match than limit.
		Truncated bool `json:"truncated" doc:"More requests match than the limit: narrow the period"`
	}
}

type archiveInput struct {
	Body struct {
		Requests []string `json:"requests" minItems:"1" maxItems:"50" uniqueItems:"true" doc:"The IDs of the requests whose bundles the archive holds"`
	}
}

// evidenceMaker makes bundles.
type evidenceMaker struct {
	svc     *requests.Service
	s       *store.Store
	reg     *repositories.Registry
	version string
	log     *slog.Logger
	now     func() time.Time
}

func registerEvidence(a huma.API, svc *requests.Service, s *store.Store, reg *repositories.Registry, version string, log *slog.Logger) {
	e := &evidenceMaker{svc: svc, s: s, reg: reg, version: version, log: log, now: time.Now}
	binary := &huma.Schema{Type: "string", Format: "binary"}

	huma.Register(a, huma.Operation{
		OperationID: "get-request-evidence", Method: http.MethodGet, Path: Prefix + "/requests/{id}/evidence",
		Summary: "Download a request's evidence bundle, in JSON or as a PDF summary (auditors, platform admins, its requester and owners)",
		Description: "The JSON bundle is the evidence: verify it offline with quai audit verify --bundle and the checkpoints' public key. " +
			"Its proof, the segments of the audit log up to a signed checkpoint, is for auditors of all repositories and platform admins " +
			"signed in (docs/design/09-security-and-compliance.md#evidence-bundle). Each download is an audit event.",
		Tags: []string{"Evidence"},
		Responses: map[string]*huma.Response{"200": {
			Description: "The bundle, or its PDF summary",
			Content:     map[string]*huma.MediaType{"application/json": {Schema: binary}, "application/pdf": {Schema: binary}},
		}},
	}, func(ctx context.Context, in *evidenceInput) (*huma.StreamResponse, error) {
		rid, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, huma.Error404NotFound("no such request")
		}
		r, err := svc.Get(ctx, rid)
		if err != nil {
			return nil, Problem(err, "path")
		}
		if _, err := authorize(ctx, authz.ReadEvidence, svc.BreakGlassObject(ctx, r)); err != nil {
			return nil, err
		}
		omitted := proofOmitted(ctx)
		b, err := e.bundle(ctx, r, omitted)
		if err != nil {
			return nil, err
		}
		name, data, err := evidenceFile(b, in.Format)
		if err != nil {
			return nil, err
		}
		if err := e.downloaded(ctx, r.ID.String(), in.Format, []db.Request{r}, omitted == ""); err != nil {
			return nil, err
		}
		return &huma.StreamResponse{Body: func(hc huma.Context) {
			hc.SetHeader("Content-Type", contentTypeOf(in.Format))
			hc.SetHeader("Content-Disposition", `attachment; filename="`+name+`"`)
			hc.SetHeader("Cache-Control", "no-store")
			hc.SetHeader("X-Content-Type-Options", "nosniff")
			hc.SetStatus(http.StatusOK)
			_, _ = hc.BodyWriter().Write(data)
		}}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "list-evidence-requests", Method: http.MethodGet, Path: Prefix + "/evidence/requests",
		Summary:     "List the requests of an audit sample: by period, repository, state and break-glass (auditors)",
		Description: "Oldest first, at most limit. An auditor of a repository lists its requests only.",
		Tags:        []string{"Evidence"},
	}, func(ctx context.Context, in *sampleInput) (*sampleOutput, error) {
		if _, err := authorize(ctx, authz.SampleEvidence, authz.Object{Repository: in.Repository}); err != nil {
			return nil, err
		}
		if err := e.validSample(in); err != nil {
			return nil, err
		}
		rows, err := s.ListEvidenceRequests(ctx, db.ListEvidenceRequestsParams{Since: pgtype.Timestamptz{Time: in.Since, Valid: true},
			Until: pgtype.Timestamptz{Time: in.Until, Valid: true}, State: in.State, Repository: in.Repository, BreakGlass: in.BreakGlass,
			MaxRows: int32(in.Limit + 1)}) // #nosec G115 -- limit is at most maxSample
		if err != nil {
			return nil, err
		}
		out := &sampleOutput{}
		out.Body.Requests = []EvidenceRequestView{}
		out.Body.Truncated = len(rows) > in.Limit
		for _, row := range rows[:min(len(rows), in.Limit)] {
			r := row.Request
			out.Body.Requests = append(out.Body.Requests, EvidenceRequestView{ID: r.ID.String(), Number: r.Number, Repository: row.Repository,
				Target: r.Target, Kind: r.Kind, State: r.State, Requester: r.Requester, BreakGlass: row.Broken, CreatedAt: r.CreatedAt.Time})
		}
		return out, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "download-evidence-archive", Method: http.MethodPost, Path: Prefix + "/evidence/archive",
		Summary: "Download the evidence bundles of a sample's requests as one ZIP archive (auditors)",
		Description: fmt.Sprintf("Each request's JSON bundle and PDF summary, named by its number, and a manifest of the files with "+
			"their SHA-256, and of the requests left out and why: at most %d requests and %d MiB. Two archives are made at once; "+
			"a third waits (429).", maxArchive, maxArchiveBytes>>20),
		Tags: []string{"Evidence"},
		Responses: map[string]*huma.Response{"200": {
			Description: "A ZIP archive",
			Content:     map[string]*huma.MediaType{"application/zip": {Schema: binary}},
		}},
	}, func(ctx context.Context, in *archiveInput) (*huma.StreamResponse, error) {
		var sample []db.Request
		for i, id := range in.Body.Requests {
			rid, err := uuid.Parse(id)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("requests[%d]: not a request ID", i))
			}
			r, err := svc.Get(ctx, rid)
			if err != nil {
				return nil, Problem(err, fmt.Sprintf("body.requests[%d]", i))
			}
			// An auditor's, of each request's repository: not a requester's of their own.
			if _, err := authorize(ctx, authz.SampleEvidence, authz.Object{Repository: svc.Repository(ctx, r)}); err != nil {
				return nil, err
			}
			if _, err := authorize(ctx, authz.ReadEvidence, svc.BreakGlassObject(ctx, r)); err != nil {
				return nil, err
			}
			sample = append(sample, r)
		}
		select {
		case archiveSlots <- struct{}{}:
		default:
			return nil, huma.ErrorWithHeaders(huma.Error429TooManyRequests("two evidence archives are being made: try again in a minute"),
				http.Header{"Retry-After": {"60"}})
		}
		omitted := proofOmitted(ctx)
		if err := e.downloaded(ctx, "archive", "zip", sample, omitted == ""); err != nil {
			<-archiveSlots
			return nil, err
		}
		return &huma.StreamResponse{Body: func(hc huma.Context) {
			defer func() { <-archiveSlots }()
			hc.SetHeader("Content-Type", "application/zip")
			hc.SetHeader("Content-Disposition", fmt.Sprintf(`attachment; filename="quai-evidence-%s.zip"`, e.now().UTC().Format("20060102T150405Z")))
			hc.SetHeader("Cache-Control", "no-store")
			hc.SetHeader("X-Content-Type-Options", "nosniff")
			hc.SetStatus(http.StatusOK)
			if err := e.archive(hc.Context(), hc.BodyWriter(), sample, omitted); err != nil {
				// The status is sent: the archive stops short, without its manifest.
				log.Error("evidence archive", slog.Any("error", err))
			}
		}}, nil
	})
}

// proofOmitted says why the caller's bundles carry no proof; empty when they do.
func proofOmitted(ctx context.Context) string {
	id, _ := auth.FromContext(ctx)
	switch {
	case id.TokenID != "" || id.Principal.Scopes != nil:
		return proofForToken
	case !allowed(ctx, authz.ReadAudit, authz.Object{}):
		return proofForAuditors
	}
	return ""
}

func (e *evidenceMaker) validSample(in *sampleInput) error {
	switch {
	case !in.Until.After(in.Since):
		return huma.Error422UnprocessableEntity("until must be after since")
	case in.Until.Sub(in.Since) > maxPeriod:
		return huma.Error422UnprocessableEntity("a sample's period is at most 366 days")
	case in.Repository != "" && e.reg == nil:
		return huma.Error422UnprocessableEntity("no such repository")
	case in.Repository != "":
		if _, err := e.reg.Get(in.Repository); err != nil {
			return huma.Error422UnprocessableEntity("no such repository")
		}
	}
	return nil
}

func contentTypeOf(format string) string {
	if format == "pdf" {
		return "application/pdf"
	}
	return "application/json"
}

// evidenceFile is a bundle's file: its name, made of the request's number only, and its bytes.
func evidenceFile(b *evidence.Bundle, format string) (string, []byte, error) {
	name := "quai-request-" + strconv.FormatInt(b.Request.Number, 10) + "-evidence."
	if format == "pdf" {
		data, err := evidence.PDF(b)
		return name + "pdf", data, err
	}
	data, err := evidence.Marshal(b)
	return name + "json", data, err
}

// downloaded records a download in the audit log, before it is sent: who, of which requests, how
// many, in which format, with the proof or not.
func (e *evidenceMaker) downloaded(ctx context.Context, object, format string, sample []db.Request, proof bool) error {
	ids := make([]string, 0, len(sample))
	for _, r := range sample {
		ids = append(ids, r.ID.String())
	}
	return e.s.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		return audit.Add(ctx, tx, audit.Event{Action: "evidence.downloaded", Object: audit.Object{Type: "evidence", ID: object},
			Details: map[string]any{"format": format, "requests": ids, "count": len(ids), "proof": proof}})
	})
}

// manifest lists an archive's files and the requests it leaves out.
type manifest struct {
	BundleVersion int             `json:"bundleVersion"`
	GeneratedAt   string          `json:"generatedAt"`
	Quai          string          `json:"quai"`
	Proof         bool            `json:"proof"`
	Files         []manifestFile  `json:"files"`
	LeftOut       []manifestEntry `json:"leftOut"`
}

type manifestFile struct {
	Name    string `json:"name"`
	Request string `json:"request"`
	Number  int64  `json:"number"`
	Bytes   int    `json:"bytes"`
	SHA256  string `json:"sha256"`
}

type manifestEntry struct {
	Request string `json:"request"`
	Number  int64  `json:"number"`
	Reason  string `json:"reason"`
}

// archive writes the sample's bundles and their PDF summaries, a request at a time, then the
// manifest.
func (e *evidenceMaker) archive(ctx context.Context, w io.Writer, sample []db.Request, omitted string) error {
	z := zip.NewWriter(w)
	m := manifest{BundleVersion: evidence.Version, GeneratedAt: audit.Time(e.now()), Quai: e.version, Proof: omitted == "",
		Files: []manifestFile{}, LeftOut: []manifestEntry{}}
	total := 0
	for _, r := range sample {
		b, err := e.bundle(ctx, r, omitted)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e.log.Error("evidence bundle", slog.String("request", r.ID.String()), slog.Any("error", err))
			m.LeftOut = append(m.LeftOut, manifestEntry{Request: r.ID.String(), Number: r.Number, Reason: "its bundle could not be made: Quai's log says why"})
			continue
		}
		var files [][2]any
		size := 0
		for _, format := range []string{"json", "pdf"} {
			_, data, err := evidenceFile(b, format)
			if err != nil {
				return err
			}
			files = append(files, [2]any{fmt.Sprintf("request-%d.%s", r.Number, format), data})
			size += len(data)
		}
		if total+size > maxArchiveBytes {
			m.LeftOut = append(m.LeftOut, manifestEntry{Request: r.ID.String(), Number: r.Number,
				Reason: fmt.Sprintf("the archive would pass %d MiB: download it alone, or in another archive", maxArchiveBytes>>20)})
			continue
		}
		total += size
		for _, f := range files {
			name, data := f[0].(string), f[1].([]byte)
			if err := zipFile(z, name, data, e.now()); err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			m.Files = append(m.Files, manifestFile{Name: name, Request: r.ID.String(), Number: r.Number, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])})
		}
	}
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	if err := zipFile(z, "manifest.json", append(data, '\n'), e.now()); err != nil {
		return err
	}
	return z.Close()
}

func zipFile(z *zip.Writer, name string, data []byte, at time.Time) error {
	f, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: at.UTC()})
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}

// Notes every bundle carries.
const (
	noteLeftOut = "Left out: the addresses people came from, people's email addresses, the plans' values, and secrets of any kind."
	noteBefore  = "A change's content before is what the request was planned on; the change set's hash covers the content after."
)

// bundle makes a request's bundle from one snapshot of its rows and of the audit log, but the
// rules' approvals, which the request's reviews decide as the request page shows them, and the
// merge commit's trailers, which Quai's mirror of the repository has.
func (e *evidenceMaker) bundle(ctx context.Context, r db.Request, omitted string) (*evidence.Bundle, error) {
	_, outcome, err := e.svc.Reviews(ctx, r)
	if err != nil {
		return nil, err
	}
	tx, err := e.s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)
	if r, err = q.GetRequest(ctx, r.ID); err != nil {
		return nil, err
	}
	b := &evidence.Bundle{BundleVersion: evidence.Version, GeneratedAt: audit.Time(e.now()), Quai: e.version,
		ChangeSets: []evidence.ChangeSet{}, Plans: []evidence.Run{}, Approvals: []evidence.Approval{}, Applies: []evidence.Run{},
		Notes: []string{noteLeftOut, noteBefore}}
	if err := evidenceRequestOf(r, outcome, &b.Request); err != nil {
		return nil, err
	}
	sets, err := q.ListChangeSets(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	for _, cs := range sets {
		changes, err := q.ListChanges(ctx, cs.ID)
		if err != nil {
			return nil, err
		}
		v := evidence.ChangeSet{Repository: cs.Repository, Hash: cs.Hash, Commit: cs.Commit, Changes: []evidence.Change{}}
		for _, c := range changes {
			v.Changes = append(v.Changes, evidence.NewChange(plan.Change{Path: c.Path, Op: c.Op, Before: c.Before, After: c.After}))
		}
		b.ChangeSets = append(b.ChangeSets, v)
	}
	if len(sets) > 0 {
		b.Request.Repository = sets[0].Repository
	}
	if err := e.reviews(ctx, q, r, b); err != nil {
		return nil, err
	}
	for kind, out := range map[string]*[]evidence.Run{"plan": &b.Plans, "apply": &b.Applies} {
		runs, err := q.LatestRequestRuns(ctx, db.LatestRequestRunsParams{RequestID: pgtype.UUID{Bytes: r.ID, Valid: true}, Kind: kind})
		if err != nil {
			return nil, err
		}
		for _, run := range runs {
			*out = append(*out, runOf(runView(run)))
		}
	}
	switch d, err := q.GetDelivery(ctx, r.ID); {
	case err == nil:
		b.Delivery = &evidence.Delivery{Branch: d.Branch, PullRequest: d.PullRequest, URL: d.Url, Head: d.Head, MergeCommit: d.MergeCommit, Trailers: []string{}}
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	case len(sets) > 0 && sets[0].Commit != "":
		// A direct commit.
		b.Delivery = &evidence.Delivery{MergeCommit: sets[0].Commit, Trailers: []string{}}
	}
	if err := breakGlassOfBundle(ctx, q, r, b); err != nil {
		return nil, err
	}
	a, err := evidence.Prove(ctx, q, r.ID.String(), omitted)
	if err != nil {
		return nil, err
	}
	b.Events, b.Personal, b.Proof = a.Events, a.Personal, a.Proof
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	e.trailers(ctx, b)
	if omitted != "" {
		b.Notes = append(b.Notes, omitted)
	}
	return b, nil
}

// evidenceRequestOf fills the bundle's request.
func evidenceRequestOf(r db.Request, outcome policy.Outcome, out *evidence.Request) error {
	var rules []policy.Rule
	if len(r.Rules) > 0 {
		if err := json.Unmarshal(r.Rules, &rules); err != nil {
			return fmt.Errorf("request %s's rules: %w", r.ID, err)
		}
	}
	sep, err := json.Marshal(separationView(requests.SeparationOf(r)))
	if err != nil {
		return err
	}
	*out = evidence.Request{ID: r.ID.String(), Number: r.Number, Kind: r.Kind, Action: r.Action, Target: r.Target, Type: r.TypeName,
		Blueprint: r.Blueprint, Stack: r.Stack, Requester: r.Requester, Reason: r.Reason, State: r.State, Inputs: r.Inputs,
		CreatedAt: audit.Time(r.CreatedAt.Time), UpdatedAt: audit.Time(r.UpdatedAt.Time), DefinitionsCommit: r.DefinitionsCommit,
		BaseCommit: r.BaseCommit, Expiry: r.Expiry, StepUp: r.StepUp, Owners: nonNilStrings(r.Owners), Rules: []evidence.Rule{}, Separation: sep}
	if len(out.Inputs) == 0 {
		out.Inputs = json.RawMessage(`{}`)
	}
	if r.SubmittedAt.Valid {
		out.SubmittedAt = audit.Time(r.SubmittedAt.Time)
	}
	if r.ExpiresAt.Valid {
		out.ExpiresAt = audit.Time(r.ExpiresAt.Time)
	}
	for _, v := range ruleViews(rules, outcome.Rules) {
		out.Rules = append(out.Rules, evidence.Rule{Names: v.Names, Approvers: v.Approvers, Min: v.Min, External: v.External, When: v.When,
			NotApplicable: v.NotApplicable, Overall: v.Overall, Approvals: v.Approvals, OnBehalfOf: v.OnBehalfOf, Satisfied: v.Satisfied})
	}
	return nil
}

// authTime is an authentication time as the audit log has it: to the second.
func authTime(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

// reviews fills the bundle's approvals: every review, those voided too, with the delegation of
// those on someone's behalf.
func (e *evidenceMaker) reviews(ctx context.Context, q *db.Queries, r db.Request, b *evidence.Bundle) error {
	reviews, err := q.ListReviews(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, rv := range reviews {
		a := evidence.Approval{Reviewer: rv.Reviewer, Groups: nonNilStrings(rv.Groups), Decision: rv.Decision, Comment: rv.Comment,
			ChangeSet: rv.ChangeSetHash, At: audit.Time(rv.CreatedAt.Time), AuthTime: authTime(rv.ReviewerAuthTime), ACR: rv.ReviewerAcr,
			StepUp: rv.StepUp, OnBehalfOf: rv.OnBehalfOf, Voided: rv.VoidedAt.Valid}
		if rv.DelegationID.Valid {
			d, err := q.GetDelegation(ctx, uuid.UUID(rv.DelegationID.Bytes))
			if err != nil {
				return fmt.Errorf("review %s's delegation: %w", rv.ID, err)
			}
			a.Delegation = &evidence.Delegation{ID: d.ID.String(), Delegator: d.Delegator, Delegate: d.Delegate, StartsAt: audit.Time(d.StartsAt.Time),
				EndsAt: audit.Time(d.EndsAt.Time), CreatedAt: audit.Time(d.CreatedAt.Time)}
			if d.RevokedAt.Valid {
				a.Delegation.RevokedAt = audit.Time(d.RevokedAt.Time)
			}
		}
		b.Approvals = append(b.Approvals, a)
	}
	return nil
}

// runOf is a run as a bundle has it: never the plan's values.
func runOf(v RunView) evidence.Run {
	out := evidence.Run{Stack: v.Stack, Commit: v.Commit, Status: v.Status, URL: v.URL, StartedAt: audit.Time(v.StartedAt), Policies: []evidence.Policy{}}
	if v.FinishedAt != nil {
		out.FinishedAt = audit.Time(*v.FinishedAt)
	}
	if s := v.Summary; s != nil {
		out.Summary = &evidence.Summary{Add: s.Add, Change: s.Change, Destroy: s.Destroy}
	}
	for _, p := range v.Policies {
		out.Policies = append(out.Policies, evidence.Policy{Name: p.Name, Result: p.Result, Message: p.Message})
	}
	if c := v.Cost; c != nil {
		out.Cost = &evidence.Cost{Currency: c.Currency, MonthlyDelta: c.MonthlyDelta}
	}
	changes := func(in []PlanChange) []evidence.PlanChange {
		var out []evidence.PlanChange
		for _, c := range in {
			out = append(out, evidence.PlanChange{Address: c.Address, Action: c.Action, Attributes: c.Attributes})
		}
		return out
	}
	if p := v.Plan; p != nil {
		for _, r := range p.Resources {
			out.Resources = append(out.Resources, evidence.PlanResource{Resource: r.Resource, Changes: changes(r.Changes)})
		}
		out.Unmapped = changes(p.Unmapped)
		for _, a := range p.Ambiguous {
			out.Unmapped = append(out.Unmapped, changes([]PlanChange{a.PlanChange})...)
		}
	}
	return out
}

// breakGlassOfBundle fills the bundle's break-glass and its post-incident review, and says so
// while the review is open.
func breakGlassOfBundle(ctx context.Context, q *db.Queries, r db.Request, b *evidence.Bundle) error {
	bg, err := q.GetBreakGlass(ctx, r.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	signs, err := q.ListBreakGlassSignOffs(ctx, r.ID)
	if err != nil {
		return err
	}
	v := &evidence.BreakGlass{Breaker: bg.Breaker, At: audit.Time(bg.At.Time), AuthTime: authTime(bg.AuthTime), ACR: bg.Acr, StepUp: bg.StepUp,
		Reason: bg.Reason, Incident: bg.Incident, ChangeSet: bg.ChangeSetHash, Head: bg.Head, FromState: bg.FromState, WindowHeld: r.WindowHeldAt.Valid,
		Review: evidence.BreakGlassRev{Open: !bg.ClosedAt.Valid, Needed: requests.SignOffsNeeded, SignOffs: []evidence.SignOff{}}}
	if bg.ClosedAt.Valid {
		v.Review.ClosedAt = audit.Time(bg.ClosedAt.Time)
	}
	for _, s := range signs {
		v.Review.SignOffs = append(v.Review.SignOffs, evidence.SignOff{Signer: s.Signer, Capacity: s.Capacity, Comment: s.Comment, At: audit.Time(s.At.Time),
			AuthTime: authTime(s.AuthTime), StepUp: s.StepUp})
	}
	b.BreakGlass = v
	if v.Review.Open {
		b.Notes = append(b.Notes, fmt.Sprintf("The request was merged by break-glass and its post-incident review is open: %d of %d sign-offs. "+
			"Download the bundle again once it is closed.", len(signs), requests.SignOffsNeeded))
	}
	return nil
}

// trailers adds the merge commit's trailers as Quai's mirror has them, fetching once when it
// lacks the commit; the bundle says so when it cannot.
func (e *evidenceMaker) trailers(ctx context.Context, b *evidence.Bundle) {
	d := b.Delivery
	if d == nil || d.MergeCommit == "" {
		return
	}
	lacks := fmt.Sprintf("The merge commit %s is not in Quai's mirror of the repository: its trailers are left out.", d.MergeCommit)
	if e.reg == nil {
		b.Notes = append(b.Notes, lacks)
		return
	}
	repo, err := e.reg.Get(b.Request.Repository)
	if err != nil || repo.Mirror == nil {
		b.Notes = append(b.Notes, lacks)
		return
	}
	trailers, err := repo.Mirror.Trailers(ctx, d.MergeCommit)
	if errors.Is(err, gitx.ErrNotFound) {
		if err = repo.Mirror.Fetch(ctx); err == nil {
			trailers, err = repo.Mirror.Trailers(ctx, d.MergeCommit)
		}
	}
	if err != nil {
		e.log.WarnContext(ctx, "evidence trailers", slog.String("request", b.Request.ID), slog.Any("error", err))
		b.Notes = append(b.Notes, lacks)
		return
	}
	d.Trailers = withoutAddresses(trailers)
}

// emailInTrailer is an address in a trailer (Requested-by: bob <bob@example.com>).
var emailInTrailer = regexp.MustCompile(`<[^<>\s]*@[^<>\s]*>`)

// withoutAddresses replaces the email addresses in a merge commit's trailers: a bundle carries no
// one's email address (docs/design/09-security-and-compliance.md#evidence-bundle).
func withoutAddresses(trailers []string) []string {
	out := make([]string, 0, len(trailers))
	for _, t := range trailers {
		out = append(out, emailInTrailer.ReplaceAllString(t, "<address left out>"))
	}
	return out
}
