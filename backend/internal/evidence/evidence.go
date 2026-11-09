// Package evidence makes and verifies a request's evidence bundle
// (docs/design/09-security-and-compliance.md#evidence-bundle): what was asked and why, what it
// changed, who approved it and how they had signed in, how it was merged and applied, and the
// request's audit events, with the records of the log that prove them against a signed checkpoint.
//
// A bundle is one JSON document, deterministic and versioned (bundleVersion). Verify checks one
// offline, with the checkpoint key's public keys: it needs nothing but the bundle. PDF draws a
// summary of one for people; the JSON is the evidence.
package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/damhau/quai/backend/internal/audit"
)

// Version is the version of the bundle's format this code writes and reads.
const Version = 1

// MaxProofRecords bounds the records of the log a bundle carries to prove its events: beyond it,
// the events of the segments left out are unproven, and the full log proves them
// (quai audit export, quai audit verify).
const MaxProofRecords = 5000

// MaxSize bounds the bundle Parse reads.
const MaxSize = 256 << 20

// Bundle is a request's evidence. Times are UTC, to the millisecond (audit.AtFormat).
type Bundle struct {
	BundleVersion int    `json:"bundleVersion"`
	GeneratedAt   string `json:"generatedAt"`
	// Quai is the version of Quai that made it.
	Quai       string      `json:"quai"`
	Request    Request     `json:"request"`
	ChangeSets []ChangeSet `json:"changeSets"`
	// Plans are the latest plan of each stack, Applies the latest apply.
	Plans      []Run       `json:"plans"`
	Approvals  []Approval  `json:"approvals"`
	Delivery   *Delivery   `json:"delivery,omitempty"`
	Applies    []Run       `json:"applies"`
	BreakGlass *BreakGlass `json:"breakGlass,omitempty"`
	// Events are the request's audit events as the log holds them: personal data as pseudonyms.
	Events []audit.Record `json:"events"`
	// Personal are the clear values of the events' personal data, with the salts that make their
	// pseudonyms: never the addresses people came from. A value erased from the log is absent.
	Personal map[string]PersonalValue `json:"personal"`
	Proof    Proof                    `json:"proof"`
	// Notes say what the bundle leaves out, and why.
	Notes []string `json:"notes"`
}

// Request is the request as Quai holds it when the bundle is made.
type Request struct {
	ID                string          `json:"id"`
	Number            int64           `json:"number"`
	Kind              string          `json:"kind"`
	Action            string          `json:"action,omitempty"`
	Repository        string          `json:"repository"`
	Target            string          `json:"target"`
	Type              string          `json:"type,omitempty"`
	Blueprint         string          `json:"blueprint,omitempty"`
	Stack             string          `json:"stack,omitempty"`
	Requester         string          `json:"requester"`
	Reason            string          `json:"reason"`
	State             string          `json:"state"`
	Inputs            json.RawMessage `json:"inputs"`
	CreatedAt         string          `json:"createdAt"`
	SubmittedAt       string          `json:"submittedAt,omitempty"`
	UpdatedAt         string          `json:"updatedAt"`
	DefinitionsCommit string          `json:"definitionsCommit"`
	BaseCommit        string          `json:"baseCommit,omitempty"`
	Expiry            string          `json:"expiry,omitempty"`
	ExpiresAt         string          `json:"expiresAt,omitempty"`
	StepUp            bool            `json:"stepUp"`
	Owners            []string        `json:"owners"`
	Rules             []Rule          `json:"rules"`
	// Separation is the separation of duties the request was planned with, as the API shows it.
	Separation json.RawMessage `json:"separation,omitempty"`
}

// Rule is an approval rule of the request, and its approvals when the bundle was made.
type Rule struct {
	Names         []string          `json:"names"`
	Approvers     []string          `json:"approvers"`
	Min           int               `json:"min"`
	External      string            `json:"external,omitempty"`
	When          string            `json:"when,omitempty"`
	NotApplicable bool              `json:"notApplicable,omitempty"`
	Overall       bool              `json:"overall,omitempty"`
	Approvals     []string          `json:"approvals"`
	OnBehalfOf    map[string]string `json:"onBehalfOf,omitempty"`
	Satisfied     bool              `json:"satisfied"`
}

// ChangeSet is what the request changes in a repository: its hash is plan.Hash of its changes.
type ChangeSet struct {
	Repository string   `json:"repository"`
	Hash       string   `json:"hash"`
	Commit     string   `json:"commit,omitempty"`
	Changes    []Change `json:"changes"`
}

// Change is a file the request changes: its content before and after, in UTF-8, or in base64
// when Encoding says so, and the diff between them.
type Change struct {
	Path        string   `json:"path"`
	Op          string   `json:"op"`
	Encoding    string   `json:"encoding,omitempty"`
	Before      string   `json:"before"`
	After       string   `json:"after"`
	AfterSHA256 string   `json:"afterSha256"`
	Diff        []string `json:"diff"`
}

// Base64 is the encoding of a change whose content is not UTF-8.
const Base64 = "base64"

// Run is a plan or an apply a runner reported.
type Run struct {
	Stack      string         `json:"stack"`
	Commit     string         `json:"commit"`
	Status     string         `json:"status"`
	URL        string         `json:"url,omitempty"`
	StartedAt  string         `json:"startedAt"`
	FinishedAt string         `json:"finishedAt,omitempty"`
	Summary    *Summary       `json:"summary,omitempty"`
	Policies   []Policy       `json:"policies"`
	Cost       *Cost          `json:"cost,omitempty"`
	Resources  []PlanResource `json:"resources,omitempty"`
	Unmapped   []PlanChange   `json:"unmapped,omitempty"`
}

// Summary counts a run's resource changes.
type Summary struct {
	Add     int `json:"add"`
	Change  int `json:"change"`
	Destroy int `json:"destroy"`
}

// Policy is a policy check's result.
type Policy struct {
	Name    string `json:"name"`
	Result  string `json:"result"`
	Message string `json:"message,omitempty"`
}

// Cost is a plan's cost estimate.
type Cost struct {
	Currency     string  `json:"currency"`
	MonthlyDelta float64 `json:"monthlyDelta"`
}

// PlanResource is a Quai resource and what a plan changes of it: never the values.
type PlanResource struct {
	Resource string       `json:"resource"`
	Changes  []PlanChange `json:"changes"`
}

// PlanChange is what a plan does to one Terraform resource.
type PlanChange struct {
	Address    string   `json:"address"`
	Action     string   `json:"action"`
	Attributes []string `json:"attributes,omitempty"`
}

// Approval is a review of the request: who, with which authentication, on whose behalf.
type Approval struct {
	Reviewer   string      `json:"reviewer"`
	Groups     []string    `json:"groups"`
	Decision   string      `json:"decision"`
	Comment    string      `json:"comment,omitempty"`
	ChangeSet  string      `json:"changeSet"`
	At         string      `json:"at"`
	AuthTime   string      `json:"authTime,omitempty"`
	ACR        string      `json:"acr,omitempty"`
	StepUp     bool        `json:"stepUp"`
	OnBehalfOf string      `json:"onBehalfOf,omitempty"`
	Delegation *Delegation `json:"delegation,omitempty"`
	Voided     bool        `json:"voided"`
}

// Delegation is the delegation a review on someone's behalf used.
type Delegation struct {
	ID        string `json:"id"`
	Delegator string `json:"delegator"`
	Delegate  string `json:"delegate"`
	StartsAt  string `json:"startsAt"`
	EndsAt    string `json:"endsAt"`
	CreatedAt string `json:"createdAt"`
	RevokedAt string `json:"revokedAt,omitempty"`
}

// Delivery is how the change reached Git: its pull request (no branch for a direct commit), its
// merge (or direct) commit, and that commit's trailers as Quai's mirror has it.
type Delivery struct {
	Branch      string   `json:"branch,omitempty"`
	PullRequest string   `json:"pullRequest,omitempty"`
	URL         string   `json:"url,omitempty"`
	Head        string   `json:"head,omitempty"`
	MergeCommit string   `json:"mergeCommit,omitempty"`
	Trailers    []string `json:"trailers"`
}

// BreakGlass is the record of a request merged by break-glass, in place of the approvals it
// lacked, and its post-incident review.
type BreakGlass struct {
	Breaker    string        `json:"breaker"`
	At         string        `json:"at"`
	AuthTime   string        `json:"authTime,omitempty"`
	ACR        string        `json:"acr,omitempty"`
	StepUp     bool          `json:"stepUp"`
	Reason     string        `json:"reason"`
	Incident   string        `json:"incident"`
	ChangeSet  string        `json:"changeSet"`
	Head       string        `json:"head,omitempty"`
	FromState  string        `json:"fromState"`
	WindowHeld bool          `json:"windowHeld"`
	Review     BreakGlassRev `json:"review"`
}

// BreakGlassRev is a break-glass's post-incident review: open until its sign-offs close it.
type BreakGlassRev struct {
	Open     bool      `json:"open"`
	ClosedAt string    `json:"closedAt,omitempty"`
	Needed   int       `json:"needed"`
	SignOffs []SignOff `json:"signOffs"`
}

// SignOff is a sign-off of a post-incident review.
type SignOff struct {
	Signer   string `json:"signer"`
	Capacity string `json:"capacity"`
	Comment  string `json:"comment,omitempty"`
	At       string `json:"at"`
	AuthTime string `json:"authTime,omitempty"`
	StepUp   bool   `json:"stepUp"`
}

// PersonalValue is the clear value of a pseudonym and its salt, in base64: audit.PersonalHolds
// checks one against the other.
type PersonalValue struct {
	Value string `json:"value"`
	Salt  string `json:"salt"`
}

// Proof proves the request's events: each segment is the log's records from the request's first
// event in it to a signed checkpoint, those of other events included as the log holds them. The
// request's own events are in Events, not repeated. An event in no segment is unproven.
type Proof struct {
	Segments []Segment  `json:"segments"`
	Unproven []Unproven `json:"unproven"`
	// Omitted, when set, says why the bundle carries no segment.
	Omitted string `json:"omitted,omitempty"`
	// Bound is MaxProofRecords when the bundle was made.
	Bound int `json:"bound"`
}

// Segment is the records of the log from From to To, a checkpoint's seq, but the request's own.
type Segment struct {
	From       int64            `json:"from"`
	To         int64            `json:"to"`
	Checkpoint audit.Checkpoint `json:"checkpoint"`
	Records    []audit.Record   `json:"records"`
}

// Unproven is an event no segment proves, and why.
type Unproven struct {
	Seq    int64  `json:"seq"`
	Reason string `json:"reason"`
}

// Marshal writes a bundle: the same bundle, the same bytes.
func Marshal(b *Bundle) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(b); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Parse reads a bundle strictly: one JSON document of this version, no field it does not know.
func Parse(r io.Reader) (*Bundle, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSize {
		return nil, fmt.Errorf("the bundle is larger than %d MiB", MaxSize>>20)
	}
	var probe struct {
		BundleVersion *int `json:"bundleVersion"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("not a bundle: %w", err)
	}
	switch {
	case probe.BundleVersion == nil:
		return nil, errors.New("not a bundle: it has no bundleVersion")
	case *probe.BundleVersion != Version:
		return nil, fmt.Errorf("a bundle of version %d: this version of quai reads version %d", *probe.BundleVersion, Version)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var b Bundle
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("not a bundle: %w", err)
	}
	if dec.More() {
		return nil, errors.New("not a bundle: more than one JSON document")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("not a bundle: something follows the document")
	}
	return &b, nil
}
