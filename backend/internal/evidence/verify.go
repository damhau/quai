package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/engine/plan"
)

// Report is what Verify found.
type Report struct {
	// Request names the request: #number and ID.
	Request string
	// Events is how many of the request's events the bundle holds, Proven how many a segment
	// proves against a checkpoint that verifies.
	Events, Proven int
	// Records is how many records of other events the segments hold.
	Records int
	// Checkpoints are the seqs of the checkpoints that verified.
	Checkpoints []int64
	// Unproven are the events no segment proves, and why.
	Unproven []Unproven
	// Failures say what does not hold: the bundle was changed, or is not what Quai wrote.
	Failures []string
	// Notes say what could not be checked, and why.
	Notes []string
}

// OK reports whether nothing failed; unproven events are not failures.
func (r Report) OK() bool { return len(r.Failures) == 0 }

// Verify checks a bundle offline with the public keys of the checkpoints: each event's hash and
// that it is the request's, each segment's chain from its first record to its checkpoint and the
// checkpoint's signature, that a segment leaves out none of the request's events, the clear
// personal values against their pseudonyms, each change set's hash and diff against its content,
// and the summary against the events where they overlap: the submission, the state, the
// approvals, the merge and the break-glass.
func Verify(b *Bundle, keys audit.Keys) Report {
	v := &verifier{b: b, keys: keys}
	v.rep.Request = fmt.Sprintf("#%d %s", b.Request.Number, b.Request.ID)
	v.events()
	v.personalValues()
	v.segments()
	v.changeSets()
	v.summary()
	return v.rep
}

type verifier struct {
	b    *Bundle
	keys audit.Keys
	rep  Report
	own  map[int64]audit.Record
}

func (v *verifier) fail(format string, args ...any) {
	v.rep.Failures = append(v.rep.Failures, fmt.Sprintf(format, args...))
}

func (v *verifier) note(format string, args ...any) {
	v.rep.Notes = append(v.rep.Notes, fmt.Sprintf(format, args...))
}

// events checks the request's events: theirs, in order, each hash its content's.
func (v *verifier) events() {
	v.own = map[int64]audit.Record{}
	v.rep.Events = len(v.b.Events)
	if len(v.b.Events) == 0 {
		v.fail("the bundle holds none of the request's audit events")
	}
	last := int64(0)
	for _, e := range v.b.Events {
		switch {
		case !About(e, v.b.Request.ID):
			v.fail("event seq %d (%s) is not about request %s", e.Seq, e.Action, v.b.Request.ID)
		case e.Seq <= last:
			v.fail("event seq %d comes again or out of order", e.Seq)
		}
		if h, err := e.Digest(); err != nil || h != e.Hash {
			v.fail("event seq %d (%s): its hash is not the hash of its content: the event was changed", e.Seq, e.Action)
		}
		last = max(last, e.Seq)
		v.own[e.Seq] = e
	}
}

var pseudonymRe = regexp.MustCompile(`^pd:[0-9a-f]{64}$`)

// personalValues checks each clear value against its pseudonym, which an event names.
func (v *verifier) personalValues() {
	var named bytes.Buffer
	for _, e := range v.b.Events {
		named.Write(e.Actor)
		named.Write(e.Object)
		named.Write(e.Details)
	}
	for _, pd := range sortedKeys(v.b.Personal) {
		p := v.b.Personal[pd]
		salt, err := base64.StdEncoding.DecodeString(p.Salt)
		switch {
		case !pseudonymRe.MatchString(pd):
			v.fail("personal: %q is not a pseudonym", clip(pd))
		case err != nil || !audit.PersonalHolds(pd, salt, p.Value):
			v.fail("personal: the clear value given for %s is not the one its pseudonym was made of", pd)
		case !bytes.Contains(named.Bytes(), []byte(`"`+pd+`"`)):
			v.fail("personal: %s is named by no event of the request", pd)
		}
	}
}

// segments checks each segment from its first record to its checkpoint.
func (v *verifier) segments() {
	proven := map[int64]bool{}
	prevTo := int64(0)
	for i, s := range v.b.Proof.Segments {
		name := fmt.Sprintf("segment %d (seq %d to %d)", i+1, s.From, s.To)
		if s.From < 1 || s.From > s.To || s.From <= prevTo {
			v.fail("%s: its range is empty, or overlaps the segment before", name)
			continue
		}
		prevTo = s.To
		if s.To-s.From+1 > MaxProofRecords+int64(len(v.b.Events)) {
			v.fail("%s: it spans more records than a bundle carries", name)
			continue
		}
		if ok := v.segment(name, s); !ok {
			continue
		}
		v.rep.Checkpoints = append(v.rep.Checkpoints, s.Checkpoint.Seq)
		v.rep.Records += len(s.Records)
		for seq := range v.own {
			if seq >= s.From && seq <= s.To {
				proven[seq] = true
			}
		}
	}
	reasons := map[int64]string{}
	for _, u := range v.b.Proof.Unproven {
		reasons[u.Seq] = u.Reason
	}
	for _, e := range v.b.Events {
		if proven[e.Seq] {
			v.rep.Proven++
			continue
		}
		reason := reasons[e.Seq]
		if reason == "" {
			reason = "no segment of the bundle proves it"
		}
		v.rep.Unproven = append(v.rep.Unproven, Unproven{Seq: e.Seq, Reason: reason})
	}
}

// segment checks one segment: every record from From to To, the request's own from Events,
// chained, the last one's hash the checkpoint's, and the checkpoint signed by one of the keys.
func (v *verifier) segment(name string, s Segment) bool {
	ok := true
	others := map[int64]audit.Record{}
	for _, r := range s.Records {
		switch {
		case r.Seq < s.From || r.Seq > s.To:
			v.fail("%s: it holds seq %d, outside its range", name, r.Seq)
			return false
		case others[r.Seq].Hash != "" || v.own[r.Seq].Hash != "":
			v.fail("%s: seq %d is in it twice", name, r.Seq)
			return false
		case About(r, v.b.Request.ID):
			v.fail("%s: seq %d (%s) is an event of the request that the bundle's events leave out", name, r.Seq, r.Action)
			ok = false
		}
		others[r.Seq] = r
	}
	prev := ""
	for seq := s.From; seq <= s.To; seq++ {
		r, mine := v.own[seq]
		if !mine {
			if r, mine = others[seq]; !mine {
				v.fail("%s: seq %d is missing: an event was removed", name, seq)
				return false
			}
			if h, err := r.Digest(); err != nil || h != r.Hash {
				v.fail("%s: seq %d: its hash is not the hash of its content: the record was changed", name, seq)
				return false
			}
		}
		if seq > s.From && r.Prev != prev {
			v.fail("%s: seq %d does not chain on seq %d: its prev is not that event's hash", name, seq, seq-1)
			return false
		}
		prev = r.Hash
	}
	c := s.Checkpoint
	switch err := c.Verify(v.keys); {
	case c.Seq != s.To:
		v.fail("%s: its checkpoint is of seq %d, not of its last record", name, c.Seq)
		return false
	case err != nil:
		v.fail("%s: the checkpoint at seq %d does not hold: %v", name, c.Seq, err)
		return false
	case c.Hash != prev:
		v.fail("%s: seq %d's hash is not the one the checkpoint signed: the events were changed, or the checkpoint swapped", name, c.Seq)
		return false
	}
	return ok
}

// maxDiffCells bounds the diffs a bundle carries and Verify computes again: the cells of the line
// diff's table, 8 bytes each.
const maxDiffCells = 2_000_000

// NewChange is a change as a bundle has it: its content in UTF-8, or base64 when it is not text,
// the hash of its content after, and its diff, empty when it is not text or too large to compute.
func NewChange(c plan.Change) Change {
	sum := sha256.Sum256(c.After)
	out := Change{Path: c.Path, Op: c.Op, AfterSHA256: hex.EncodeToString(sum[:]), Diff: []string{}}
	if !utf8.Valid(c.Before) || !utf8.Valid(c.After) {
		out.Encoding = Base64
		out.Before, out.After = base64.StdEncoding.EncodeToString(c.Before), base64.StdEncoding.EncodeToString(c.After)
		return out
	}
	out.Before, out.After = string(c.Before), string(c.After)
	if diffable(c) {
		out.Diff = DiffLines(c)
	}
	return out
}

func diffable(c plan.Change) bool { return lines(c.Before)*lines(c.After) <= maxDiffCells }

// changeSets checks each change's content against its hash, each diff against the content, and
// each change set's hash.
func (v *verifier) changeSets() {
	for _, cs := range v.b.ChangeSets {
		var changes []plan.Change
		for _, c := range cs.Changes {
			before, after, err := contents(c)
			if err != nil {
				v.fail("change set %s, %s: %v", cs.Repository, clip(c.Path), err)
				continue
			}
			sum := sha256.Sum256(after)
			if hex.EncodeToString(sum[:]) != c.AfterSHA256 {
				v.fail("change set %s, %s: its content after is not the one its afterSha256 names", cs.Repository, clip(c.Path))
			}
			pc := plan.Change{Path: c.Path, Op: c.Op, Before: before, After: after}
			switch {
			case c.Encoding == Base64 || !diffable(pc):
				if len(c.Diff) > 0 {
					v.fail("change set %s, %s: it has a diff, but it is not text or too large for one", cs.Repository, clip(c.Path))
				}
			case !slices.Equal(DiffLines(pc), c.Diff):
				v.fail("change set %s, %s: its diff is not the diff of its content", cs.Repository, clip(c.Path))
			}
			changes = append(changes, pc)
		}
		slices.SortFunc(changes, func(a, b plan.Change) int { return strings.Compare(a.Path, b.Path) })
		if plan.Hash(changes) != cs.Hash {
			v.fail("change set %s: its hash is not the hash of its changes: a change was edited, added or removed", cs.Repository)
		}
	}
}

func lines(b []byte) int { return bytes.Count(b, []byte("\n")) + 1 }

// contents returns a change's content before and after.
func contents(c Change) ([]byte, []byte, error) {
	switch c.Encoding {
	case "":
		if !utf8.ValidString(c.Before) || !utf8.ValidString(c.After) {
			return nil, nil, fmt.Errorf("its content is not UTF-8")
		}
		return []byte(c.Before), []byte(c.After), nil
	case Base64:
		before, err := base64.StdEncoding.DecodeString(c.Before)
		if err != nil {
			return nil, nil, fmt.Errorf("its content before is not base64: %w", err)
		}
		after, err := base64.StdEncoding.DecodeString(c.After)
		if err != nil {
			return nil, nil, fmt.Errorf("its content after is not base64: %w", err)
		}
		return before, after, nil
	}
	return nil, nil, fmt.Errorf("an unknown encoding %q", clip(c.Encoding))
}

// DiffLines is a change's diff as a bundle has it: each line after its operation (' ', '-', '+').
func DiffLines(c plan.Change) []string {
	out := []string{}
	for _, l := range c.Diff() {
		out = append(out, string(l.Op)+l.Text)
	}
	return out
}

// event is a request's event, read.
type event struct {
	audit.Record
	actorType, actorID, authTime string
	details                      map[string]any
}

func (v *verifier) read() []event {
	var out []event
	for _, e := range v.b.Events {
		var a struct {
			Type     string `json:"type"`
			ID       string `json:"id"`
			AuthTime string `json:"authTime"`
		}
		x := event{Record: e}
		if json.Unmarshal(e.Actor, &a) != nil || json.Unmarshal(e.Details, &x.details) != nil {
			v.fail("event seq %d: its actor or its details are not JSON objects", e.Seq)
			continue
		}
		x.actorType, x.actorID, x.authTime = a.Type, a.ID, a.AuthTime
		out = append(out, x)
	}
	return out
}

// same reports whether a value of an event, a pseudonym or not, is want: unknown when the
// pseudonym's clear value is not in the bundle (erased).
func (v *verifier) same(value any, want string) (equal, known bool) {
	s, _ := value.(string)
	if !strings.HasPrefix(s, audit.PersonalPrefix) {
		return s == want, true
	}
	p, ok := v.b.Personal[s]
	if !ok {
		return false, false
	}
	return p.Value == want, true
}

// check fails when an event's value is not the summary's, notes when it cannot be told.
func (v *verifier) check(e event, what string, value any, want string) bool {
	equal, known := v.same(value, want)
	switch {
	case !known:
		v.note("event seq %d (%s): its %s was erased from the log, and cannot be checked against the summary", e.Seq, e.Action, what)
		return true
	case !equal:
		v.fail("the summary's %s (%q) is not the one event seq %d (%s) records: the summary was edited", what, clip(want), e.Seq, e.Action)
		return false
	}
	return true
}

func str(d map[string]any, k string) string { s, _ := d[k].(string); return s }

// Request actions, by the state they end in.
var terminal = map[string]bool{"applied": true, "failed": true, "rejected": true, "cancelled": true, "stale": true, "expired": true}

// summary checks the summary against the events where they overlap.
func (v *verifier) summary() {
	evs := v.read()
	r := v.b.Request
	of := func(actions ...string) []event {
		var out []event
		for _, e := range evs {
			if slices.Contains(actions, strings.TrimPrefix(e.Action, "request.")) {
				out = append(out, e)
			}
		}
		return out
	}
	// The submission.
	if subs := of("submitted"); len(subs) != 1 {
		if len(evs) > 0 {
			v.fail("the bundle holds %d request.submitted events, not one", len(subs))
		}
	} else {
		e := subs[0]
		for _, f := range []struct{ what, key, want string }{
			{"kind", "kind", r.Kind}, {"action", "action", r.Action}, {"type", "type", r.Type}, {"blueprint", "blueprint", r.Blueprint},
			{"repository", "repository", r.Repository}, {"definitions commit", "definitionsCommit", r.DefinitionsCommit}, {"reason", "reason", r.Reason},
		} {
			// What an event of an earlier version of Quai does not record is not checked.
			if value, ok := e.details[f.key]; ok {
				v.check(e, f.what, value, f.want)
			}
		}
		if e.actorType == audit.User || e.actorType == audit.Token {
			v.check(e, "requester", e.actorID, r.Requester)
		}
	}
	// The state: a final one is the last state event's.
	states := of("submitted", "retried", "applied", "failed", "rejected", "cancelled", "stale", "expired")
	if n := len(states); n > 0 {
		last := strings.TrimPrefix(states[n-1].Action, "request.")
		switch {
		case terminal[r.State] && last != r.State:
			v.fail("the summary's state (%s) is not the one the events end in (event seq %d, %s): the summary was edited, or an event removed", r.State, states[n-1].Seq, states[n-1].Action)
		case !terminal[r.State] && terminal[last]:
			v.fail("the summary's state (%s) is not the one the events end in (event seq %d, %s): the summary was edited", r.State, states[n-1].Seq, states[n-1].Action)
		}
	}
	v.approvals(of("reviewed"))
	v.merge(of("merged", "merged_without_approval", "merged_outside_window"))
	v.breakGlass(of("break_glass"), of("break_glass_signed_off"), of("break_glass_review_closed"))
}

// approvals matches each review to its request.reviewed event, one for one.
func (v *verifier) approvals(reviewed []event) {
	if len(reviewed) != len(v.b.Approvals) {
		v.fail("the summary has %d reviews, the events %d request.reviewed: a review or an event was added or removed", len(v.b.Approvals), len(reviewed))
		return
	}
	used := make([]bool, len(reviewed))
	for _, a := range v.b.Approvals {
		i := -1
		for j, e := range reviewed {
			reviewer, known := v.same(e.actorID, a.Reviewer)
			if !used[j] && str(e.details, "decision") == a.Decision && str(e.details, "changeSet") == a.ChangeSet && e.authTime == a.AuthTime && (reviewer || !known) {
				i = j
				break
			}
		}
		if i < 0 {
			v.fail("the summary's review by %s (%s, at %s) is not the one any request.reviewed event records: the summary was edited", clip(a.Reviewer), a.Decision, a.At)
			continue
		}
		used[i] = true
		e := reviewed[i]
		v.check(e, "review's comment", e.details["comment"], a.Comment)
		v.check(e, "review's on behalf of", e.details["onBehalfOf"], a.OnBehalfOf)
		if d := str(e.details, "delegation"); a.Delegation != nil && d != a.Delegation.ID || a.Delegation == nil && d != "" {
			v.fail("the summary's review by %s names another delegation than event seq %d does", clip(a.Reviewer), e.Seq)
		}
		if s, ok := e.details["stepUp"].(bool); ok && s != a.StepUp {
			v.fail("the summary's review by %s says step-up %v, event seq %d %v", clip(a.Reviewer), a.StepUp, e.Seq, s)
		}
	}
}

// mergeCommit is the commit the summary says the change reached Git with.
func (b *Bundle) mergeCommit() string {
	if b.Delivery != nil && b.Delivery.MergeCommit != "" {
		return b.Delivery.MergeCommit
	}
	if len(b.ChangeSets) > 0 {
		return b.ChangeSets[0].Commit
	}
	return ""
}

func (v *verifier) merge(merged []event) {
	commit := v.b.mergeCommit()
	switch {
	case len(merged) == 0 && commit != "":
		v.fail("the summary names merge commit %s, but no event records a merge", clip(commit))
	case len(merged) > 0 && commit == "":
		last := merged[len(merged)-1]
		v.fail("event seq %d (%s) records merge commit %s, but the summary names none: the summary was edited", last.Seq, last.Action, clip(str(last.details, "commit")))
	case len(merged) > 0:
		last := merged[len(merged)-1]
		if c := str(last.details, "commit"); c != commit {
			v.fail("the summary's merge commit (%s) is not the one event seq %d (%s) records (%s): the summary was edited", clip(commit), last.Seq, last.Action, clip(c))
		}
	}
	if v.b.Delivery != nil && commit != "" && len(v.b.Delivery.Trailers) == 0 {
		v.note("the bundle carries no trailers of the merge commit: Quai's mirror did not have it")
	}
}

func (v *verifier) breakGlass(broken, signed, closed []event) {
	bg := v.b.BreakGlass
	if bg == nil {
		if len(broken) > 0 {
			v.fail("event seq %d records a break-glass, but the summary has none: the summary was edited", broken[0].Seq)
		}
		return
	}
	if len(broken) == 0 {
		v.fail("the summary has a break-glass, but no request.break_glass event records it")
		return
	}
	e := broken[len(broken)-1]
	v.check(e, "breaker", e.actorID, bg.Breaker)
	v.check(e, "break-glass's reason", e.details["reason"], bg.Reason)
	for _, f := range []struct{ what, key, want string }{
		{"break-glass's incident", "incident", bg.Incident}, {"break-glass's change set", "changeSet", bg.ChangeSet},
		{"break-glass's head", "head", bg.Head}, {"state broken from", "from", bg.FromState},
	} {
		v.check(e, f.what, e.details[f.key], f.want)
	}
	if e.authTime != bg.AuthTime {
		v.fail("the summary's break-glass authentication time (%s) is not the one event seq %d records (%s)", bg.AuthTime, e.Seq, e.authTime)
	}
	if len(signed) != len(bg.Review.SignOffs) {
		v.fail("the summary has %d sign-offs of the post-incident review, the events %d", len(bg.Review.SignOffs), len(signed))
	} else {
		for i, s := range bg.Review.SignOffs {
			v.check(signed[i], "sign-off's signer", signed[i].actorID, s.Signer)
			v.check(signed[i], "sign-off's capacity", signed[i].details["capacity"], s.Capacity)
			v.check(signed[i], "sign-off's comment", signed[i].details["comment"], s.Comment)
		}
	}
	if bg.Review.Open == (len(closed) > 0) {
		v.fail("the summary says the post-incident review is open: %v, the events: %v", bg.Review.Open, len(closed) == 0)
	}
	if d := v.b.Delivery; d != nil && len(d.Trailers) > 0 && v.b.mergeCommit() != "" {
		for _, t := range [][2]string{{"Break-glass-by", bg.Breaker}, {"Break-glass-incident", bg.Incident}} {
			if !slices.ContainsFunc(d.Trailers, func(l string) bool {
				key, value, _ := strings.Cut(l, ":")
				return key == t[0] && visible(value) == visible(t[1])
			}) {
				v.fail("the merge commit's trailers do not say %s: %q", t[0], clip(t[1]))
			}
		}
	}
}

// visible keeps a value's letters, digits, punctuation and symbols: a trailer drops what could
// end its line or hide in it.
func visible(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return r
		}
		return -1
	}, s)
}

// clip shortens what a message quotes from the bundle.
func clip(s string) string {
	if len(s) <= 120 {
		return s
	}
	return strings.ToValidUTF8(s[:120], "") + "…"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
