package evidence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"codeberg.org/go-pdf/fpdf"
	"golang.org/x/text/encoding/charmap"

	"github.com/damhau/quai/backend/internal/audit"
)

// What the PDF draws at most; the JSON has everything.
const (
	// PDFDiffLines bounds the lines of diffs the PDF draws, all changes together.
	PDFDiffLines = 400
	// PDFEvents bounds the audit events it lists.
	PDFEvents = 300
	// pdfField bounds a value, in characters, pdfLines in lines; pdfLine a line of a diff or of a
	// trailer.
	pdfField = 2000
	pdfLines = 40
	pdfLine  = 118
	// pdfContext is how many unchanged lines a diff keeps around a change.
	pdfContext = 3
)

// PDF draws a summary of a bundle for people: text in the core fonts, nothing but text, no link
// nor script. What a request's people wrote is drawn as plain text: control, direction and
// invisible characters are dropped, characters outside Windows-1252 shown as '?', and long values
// cut, the JSON bundle having them in full.
func PDF(b *Bundle) ([]byte, error) {
	p := &page{f: fpdf.New("P", "mm", "A4", "")}
	f := p.f
	f.SetCompression(true)
	f.SetCatalogSort(true)
	f.SetMargins(15, 18, 15)
	f.SetAutoPageBreak(true, 15)
	f.AliasNbPages("{nb}")
	title := fmt.Sprintf("Quai evidence bundle, request #%d", b.Request.Number)
	f.SetTitle(title, false)
	f.SetCreator("Quai", false)
	f.SetProducer("Quai", false)
	if t, err := time.Parse(audit.AtFormat, b.GeneratedAt); err == nil {
		f.SetCreationDate(t)
		f.SetModificationDate(t)
	}
	f.SetHeaderFunc(func() {
		f.SetFont("Helvetica", "", 8)
		f.SetTextColor(110, 110, 110)
		f.SetXY(15, 8)
		f.CellFormat(0, 5, enc(title), "", 0, "L", false, 0, "")
		f.SetTextColor(0, 0, 0)
		f.SetXY(15, 18)
	})
	f.SetFooterFunc(func() {
		f.SetY(-12)
		f.SetFont("Helvetica", "", 8)
		f.SetTextColor(110, 110, 110)
		f.CellFormat(0, 5, fmt.Sprintf("Page %d of {nb}", f.PageNo()), "", 0, "R", false, 0, "")
		f.SetTextColor(0, 0, 0)
	})
	f.AddPage()
	f.SetFont("Helvetica", "B", 16)
	f.CellFormat(0, 9, enc(fmt.Sprintf("Evidence bundle: request #%d", b.Request.Number)), "", 1, "L", false, 0, "")
	p.para(fmt.Sprintf("A summary of the JSON evidence bundle generated at %s by Quai %s. The JSON bundle is the evidence: "+
		"verify it offline with quai audit verify --bundle and the public key of the audit log's checkpoints. "+
		"In this summary, characters outside Windows-1252 are shown as '?', control and direction characters are dropped, "+
		"and long values are cut.", b.GeneratedAt, b.Quai))
	if bg := b.BreakGlass; bg != nil && bg.Review.Open {
		p.sub(fmt.Sprintf("Merged by break-glass: its post-incident review is OPEN, %d of %d sign-offs.", len(bg.Review.SignOffs), bg.Review.Needed))
	}

	p.request(b)
	p.approvals(b)
	p.changes(b)
	p.runs("Plans", b.Plans, true)
	p.delivery(b)
	p.runs("Applies", b.Applies, false)
	p.breakGlass(b)
	p.events(b)
	p.proof(b)
	if len(b.Notes) > 0 {
		p.heading("Notes")
		for _, n := range b.Notes {
			p.para(n)
		}
	}
	if err := f.Error(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := f.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type page struct{ f *fpdf.Fpdf }

func (p *page) heading(s string) {
	p.f.Ln(3)
	p.f.SetFont("Helvetica", "B", 12)
	p.f.CellFormat(0, 7, enc(s), "B", 1, "L", false, 0, "")
	p.f.Ln(1)
}

func (p *page) sub(s string) {
	p.f.SetFont("Helvetica", "B", 10)
	p.f.MultiCell(0, 5, enc(clean(s, pdfField)), "", "L", false)
}

func (p *page) para(s string) {
	p.f.SetFont("Helvetica", "", 9)
	p.f.MultiCell(0, 4.5, enc(clean(s, pdfField)), "", "L", false)
	p.f.Ln(1)
}

// field draws a label and its value, which may run over several lines; nothing when empty.
func (p *page) field(label, value string) {
	if value == "" {
		return
	}
	f := p.f
	f.SetX(15)
	f.SetFont("Helvetica", "B", 9)
	f.CellFormat(42, 4.5, enc(label), "", 0, "L", false, 0, "")
	f.SetFont("Helvetica", "", 9)
	f.MultiCell(0, 4.5, enc(clean(value, pdfField)), "", "L", false)
	f.SetX(15)
}

// mono draws lines in a fixed font, each cut to a line.
func (p *page) mono(lines []string) {
	p.f.SetFont("Courier", "", 7)
	for _, l := range lines {
		p.f.SetX(15)
		p.f.CellFormat(0, 3.4, enc(line(l)), "", 1, "L", false, 0, "")
	}
	p.f.Ln(1)
}

func yes(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func (p *page) request(b *Bundle) {
	r := b.Request
	p.heading("Request")
	p.field("Number", fmt.Sprintf("#%d", r.Number))
	p.field("ID", r.ID)
	kind := r.Kind
	if r.Action != "" {
		kind += " (" + r.Action + ")"
	}
	p.field("Kind", kind)
	p.field("Repository", r.Repository)
	p.field("Target", r.Target)
	p.field("Type", r.Type)
	p.field("Blueprint", r.Blueprint)
	p.field("Stack", r.Stack)
	p.field("Requester", r.Requester)
	p.field("State", r.State)
	p.field("Created", r.CreatedAt)
	p.field("Submitted", r.SubmittedAt)
	p.field("Updated", r.UpdatedAt)
	p.field("Definitions commit", r.DefinitionsCommit)
	p.field("Base commit", r.BaseCommit)
	if r.Expiry != "" {
		p.field("Approval expiry", r.Expiry+", at "+r.ExpiresAt)
	}
	p.field("Step-up to review", yes(r.StepUp))
	p.field("Owners", strings.Join(r.Owners, ", "))
	reason := r.Reason
	if reason == "" {
		reason = "(none given)"
	}
	p.field("Reason", reason)
	if len(r.Rules) > 0 {
		p.sub("Approval rules")
		for _, rule := range r.Rules {
			s := fmt.Sprintf("%s: %d of %s", strings.Join(rule.Names, ", "), rule.Min, strings.Join(rule.Approvers, ", "))
			switch {
			case rule.NotApplicable:
				s += "; the plan says it does not apply"
			case rule.Satisfied:
				s += "; satisfied by " + strings.Join(rule.Approvals, ", ")
			default:
				s += "; not satisfied"
			}
			if rule.When != "" {
				s += "; when " + rule.When
			}
			p.para(s)
		}
	}
}

func (p *page) approvals(b *Bundle) {
	p.heading("Approvals")
	if len(b.Approvals) == 0 {
		if b.BreakGlass != nil {
			p.para("No review: the request was merged by break-glass, whose record is below.")
		} else {
			p.para("No review.")
		}
	}
	for _, a := range b.Approvals {
		p.sub(fmt.Sprintf("%s by %s at %s", a.Decision, a.Reviewer, a.At))
		auth := "with an API token: no authentication time"
		if a.AuthTime != "" {
			auth = "signed in at " + a.AuthTime
			if a.ACR != "" {
				auth += ", acr " + a.ACR
			}
			auth += ", step-up " + yes(a.StepUp)
		}
		p.field("Authentication", auth)
		p.field("Groups", strings.Join(a.Groups, ", "))
		if a.OnBehalfOf != "" {
			s := "on behalf of " + a.OnBehalfOf
			if d := a.Delegation; d != nil {
				s += fmt.Sprintf(", through delegation %s from %s to %s, made %s", d.ID, d.StartsAt, d.EndsAt, d.CreatedAt)
				if d.RevokedAt != "" {
					s += ", revoked " + d.RevokedAt
				}
			}
			p.field("Delegation", s)
		}
		p.field("Change set", a.ChangeSet)
		if a.Voided {
			p.field("Voided", "yes: it counts no longer")
		}
		p.field("Comment", a.Comment)
	}
}

func (p *page) changes(b *Bundle) {
	p.heading("Changes")
	budget := PDFDiffLines
	for _, cs := range b.ChangeSets {
		p.field("Repository", cs.Repository)
		p.field("Change set", cs.Hash)
		p.field("Commit", cs.Commit)
		for _, c := range cs.Changes {
			p.sub(c.Op + " " + c.Path)
			if c.Encoding != "" {
				p.para("Not text: the JSON bundle has its content.")
				continue
			}
			lines := folded(c.Diff)
			if len(lines) > budget {
				lines = append(lines[:budget], fmt.Sprintf("... the diff is cut here: the JSON bundle has all %d lines", len(c.Diff)))
			}
			budget = max(0, budget-len(lines))
			p.mono(lines)
		}
	}
}

// folded keeps a diff's changed lines and a few around them.
func folded(diff []string) []string {
	keep := make([]bool, len(diff))
	for i, l := range diff {
		if strings.HasPrefix(l, " ") {
			continue
		}
		for j := max(0, i-pdfContext); j <= min(len(diff)-1, i+pdfContext); j++ {
			keep[j] = true
		}
	}
	var out []string
	skipped := 0
	for i, l := range diff {
		if !keep[i] {
			skipped++
			continue
		}
		if skipped > 0 {
			out = append(out, fmt.Sprintf("  ... %d unchanged lines", skipped))
			skipped = 0
		}
		out = append(out, l)
	}
	if skipped > 0 {
		out = append(out, fmt.Sprintf("  ... %d unchanged lines", skipped))
	}
	return out
}

func (p *page) runs(title string, runs []Run, plans bool) {
	p.heading(title)
	if len(runs) == 0 {
		p.para("None reported.")
	}
	for _, r := range runs {
		p.sub(fmt.Sprintf("%s: %s", r.Stack, r.Status))
		p.field("Commit", r.Commit)
		p.field("Started", r.StartedAt)
		p.field("Finished", r.FinishedAt)
		p.field("Run", r.URL)
		if s := r.Summary; s != nil {
			p.field("Summary", fmt.Sprintf("%d to add, %d to change, %d to destroy", s.Add, s.Change, s.Destroy))
		}
		for _, pol := range r.Policies {
			s := pol.Result
			if pol.Message != "" {
				s += ": " + pol.Message
			}
			p.field("Policy "+pol.Name, s)
		}
		if c := r.Cost; c != nil {
			p.field("Cost", fmt.Sprintf("%+.2f %s a month", c.MonthlyDelta, c.Currency))
		}
		if !plans {
			continue
		}
		var lines []string
		for _, res := range r.Resources {
			for _, c := range res.Changes {
				lines = append(lines, fmt.Sprintf("%s: %s %s", res.Resource, c.Action, c.Address))
			}
		}
		for _, c := range r.Unmapped {
			lines = append(lines, fmt.Sprintf("(no resource): %s %s", c.Action, c.Address))
		}
		p.mono(lines)
	}
}

func (p *page) delivery(b *Bundle) {
	p.heading("Delivery")
	d := b.Delivery
	if d == nil {
		p.para("Not delivered.")
		return
	}
	if d.PullRequest != "" {
		p.field("Pull request", d.PullRequest)
		p.field("Address", d.URL)
		p.field("Branch", d.Branch)
		p.field("Head", d.Head)
	} else if d.Branch == "" {
		p.para("A direct commit to the branch.")
	}
	p.field("Merge commit", d.MergeCommit)
	if len(d.Trailers) > 0 {
		p.sub("The merge commit's trailers")
		p.mono(d.Trailers)
	}
}

func (p *page) breakGlass(b *Bundle) {
	bg := b.BreakGlass
	if bg == nil {
		return
	}
	p.heading("Break-glass")
	p.field("Broken by", bg.Breaker)
	p.field("At", bg.At)
	auth := "signed in at " + bg.AuthTime
	if bg.ACR != "" {
		auth += ", acr " + bg.ACR
	}
	p.field("Authentication", auth+", step-up "+yes(bg.StepUp))
	p.field("Incident", bg.Incident)
	p.field("Reason", bg.Reason)
	p.field("Change set", bg.ChangeSet)
	p.field("Head", bg.Head)
	p.field("Broken from", bg.FromState)
	p.field("Change window lifted", yes(bg.WindowHeld))
	rev := bg.Review
	if rev.Open {
		p.sub(fmt.Sprintf("Post-incident review: OPEN, %d of %d sign-offs", len(rev.SignOffs), rev.Needed))
	} else {
		p.sub("Post-incident review: closed at " + rev.ClosedAt)
	}
	for _, s := range rev.SignOffs {
		auth := ""
		if s.AuthTime != "" {
			auth = ", signed in at " + s.AuthTime + ", step-up " + yes(s.StepUp)
		}
		p.field("Signed off", fmt.Sprintf("by %s as %s at %s%s", s.Signer, s.Capacity, s.At, auth))
		p.field("Comment", s.Comment)
	}
}

func (p *page) events(b *Bundle) {
	p.heading("Audit events")
	p.para("The request's events in the audit log, each with its hash: who acted is given in clear where the bundle holds the value.")
	f := p.f
	for i, e := range b.Events {
		if i == PDFEvents {
			p.para(fmt.Sprintf("... and %d more events: the JSON bundle has them all.", len(b.Events)-PDFEvents))
			break
		}
		f.SetFont("Helvetica", "", 8)
		f.SetX(15)
		f.CellFormat(14, 4, fmt.Sprintf("%d", e.Seq), "", 0, "L", false, 0, "")
		f.CellFormat(40, 4, enc(clean(e.At, 40)), "", 0, "L", false, 0, "")
		f.CellFormat(66, 4, enc(clean(e.Action, 60)), "", 0, "L", false, 0, "")
		f.CellFormat(0, 4, enc(clean(actorOf(b, e), 60)), "", 1, "L", false, 0, "")
		f.SetFont("Courier", "", 6.5)
		f.SetX(29)
		f.CellFormat(0, 3.2, "hash "+e.Hash, "", 1, "L", false, 0, "")
	}
}

// actorOf names who acted in an event, in clear when the bundle holds the value.
func actorOf(b *Bundle, e audit.Record) string {
	var a struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	_ = json.Unmarshal(e.Actor, &a)
	id := a.ID
	if v, ok := b.Personal[id]; ok {
		id = v.Value
	} else if strings.HasPrefix(id, audit.PersonalPrefix) {
		id = "(erased)"
	}
	switch a.Type {
	case audit.System:
		return "Quai"
	case audit.Runner:
		return "runner of " + id
	case audit.Token:
		return id + " (API token)"
	}
	return id
}

func (p *page) proof(b *Bundle) {
	p.heading("Proof")
	pr := b.Proof
	proven := len(b.Events) - len(pr.Unproven)
	records := 0
	for _, s := range pr.Segments {
		records += len(s.Records)
	}
	switch {
	case pr.Omitted != "":
		p.para("The bundle carries no proof: " + pr.Omitted)
	case len(pr.Segments) > 0:
		p.para(fmt.Sprintf("%d of the %d events are proven by %d segments of the audit log, holding %d records of other events, "+
			"each up to a signed checkpoint.", proven, len(b.Events), len(pr.Segments), records))
	}
	for _, s := range pr.Segments {
		p.field(fmt.Sprintf("Seq %d to %d", s.From, s.To), fmt.Sprintf("checkpoint signed at %s with key %s", s.Checkpoint.At, s.Checkpoint.Key))
	}
	if len(pr.Unproven) > 0 && pr.Omitted == "" {
		p.sub("Unproven events")
		for _, u := range pr.Unproven {
			p.field(fmt.Sprintf("Seq %d", u.Seq), u.Reason)
		}
	}
}

// clean makes text drawable: valid UTF-8 without control, direction or other invisible format
// characters, at most limit characters and pdfLines lines.
func clean(s string, limit int) string {
	s = strings.ToValidUTF8(s, "?")
	var b strings.Builder
	n, lines := 0, 0
	for _, r := range s {
		if n == limit || lines == pdfLines {
			b.WriteString("...")
			break
		}
		switch {
		case r == '\n':
			lines++
		case r == '\t':
			r = ' '
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r),
			unicode.Is(unicode.Co, r), unicode.Is(unicode.Cs, r), r == utf8.RuneError:
			continue
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// line makes a line of a diff or a trailer drawable on one line.
func line(s string) string {
	return strings.ReplaceAll(clean(s, pdfLine), "\n", " ")
}

// enc encodes text in Windows-1252, the core fonts' encoding: '?' for what it does not have.
func enc(s string) string {
	b := make([]byte, 0, len(s))
	e := charmap.Windows1252
	for _, r := range s {
		if c, ok := e.EncodeRune(r); ok && (c >= 0x20 || c == '\n') {
			b = append(b, c)
		} else {
			b = append(b, '?')
		}
	}
	return string(b)
}
