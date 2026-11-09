package requests

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Separation of duties (docs/design/05-delivery-and-approvals.md#separation-of-duties): the
// repository document's options, kept with each request as it was planned.
//
//   - definitionAuthors: who changed the request's definitions (their files, schemas included) in
//     the period before the request cannot review it. The period is measured on what Quai
//     recorded, never on commit dates: the commits between the definitions commit Quai had
//     recorded when the period began and the request's definitions commit (the whole history when
//     Quai had none). Each commit's author, committer and Co-authored-by trailers, and a merge's
//     committer, are matched to people by the directory's verified emails, and Quai's own
//     <name>@users.quai to the person of that name; whoever has one of their emails as a verified
//     email when reviewing is excluded too (open questions 71 and 72).
//   - destroys: requireTwo: the policy.RequireTwo rule, two different approvers until a plan
//     shows that nothing is destroyed (open question 73).
//
// A request keeps both as planned; when the repository's options become stricter while it waits
// for approval, it goes stale instead of being reviewed or approved (open question 74).

// Separation is the separation of duties a request was planned with: the repository's options,
// and with definitionAuthors, who changed its definitions in that period before it. Submitting
// keeps it with the request; it is never computed again.
type Separation struct {
	DefinitionAuthors repositories.Period `json:"definitionAuthors,omitzero"`
	Destroys          string              `json:"destroys,omitempty"`
	// Since is when the period began, by the database's clock; From is the definitions commit
	// Quai had recorded then, "" when it had none and the whole history counted.
	Since time.Time `json:"since,omitzero"`
	From  string    `json:"from,omitempty"`
	// Commit is the request's definitions commit, where the period ends.
	Commit string `json:"commit,omitempty"`
	// Authors are the people who cannot review, each with the commits that name them.
	Authors []DefinitionAuthor `json:"authors,omitempty"`
	// Unmatched are the identities of those commits that matched no one when the request was
	// planned. Whoever has one of their emails as a verified email when reviewing is excluded.
	Unmatched []UnmatchedIdentity `json:"unmatched,omitempty"`
	// Emails are every email those commits carry, in lower case.
	Emails []string `json:"emails,omitempty"`
}

// DefinitionAuthor is a person who changed a request's definitions in the period, and the
// commits that name them.
type DefinitionAuthor struct {
	Name    string   `json:"name"`
	Commits []string `json:"commits"`
}

// UnmatchedIdentity is an identity of the commits that matched no one, and the commits it is in.
type UnmatchedIdentity struct {
	Name    string   `json:"name,omitempty"`
	Email   string   `json:"email,omitempty"`
	Commits []string `json:"commits"`
}

func (u UnmatchedIdentity) String() string {
	switch {
	case u.Name == "":
		return "<" + u.Email + ">"
	case u.Email == "":
		return u.Name
	}
	return u.Name + " <" + u.Email + ">"
}

// Excluded are the names of the people the separation keeps from reviewing.
func (s Separation) Excluded() []string {
	out := make([]string, len(s.Authors))
	for i, a := range s.Authors {
		out[i] = a.Name
	}
	return out
}

// commitsOf returns the commits that keep a person from reviewing, or none.
func (s Separation) commitsOf(name, email string) []string {
	for _, a := range s.Authors {
		if a.Name == name {
			return a.Commits
		}
	}
	for _, u := range s.Unmatched {
		if email != "" && strings.EqualFold(u.Email, email) {
			return u.Commits
		}
	}
	return nil
}

// SeparationOf reads a request's separation; a request submitted before P4-12 has none.
func SeparationOf(r db.Request) Separation {
	var s Separation
	_ = json.Unmarshal(r.Separation, &s)
	return s
}

// fallbackApprovers are who approves a rule Quai adds when no rule names approvers: the owners,
// and whoever the repository's floor rules name.
func fallbackApprovers(repo *repositories.Repo) []string {
	approvers := []string{"owners"}
	for _, f := range repo.FloorRules {
		approvers = append(approvers, f.Approvers...)
	}
	slices.Sort(approvers)
	return slices.Compact(approvers)
}

// withSeparation applies the repository's separation of duties to a plan whose definitions are
// made of files: the requireTwo rule, and the definition authors. It comes after the plan's
// rules, before codeowners and the manager are resolved for them. When the authors cannot be
// read the plan fails: who may approve would be unknown.
func (s *Service) withSeparation(ctx context.Context, repo *repositories.Repo, p *Plan, files []string) error {
	o := repo.Document.Spec.Enforcement.SeparationOfDuties
	p.Separation = Separation{DefinitionAuthors: o.DefinitionAuthors, Destroys: o.Destroys}
	if o.Destroys == repositories.RequireTwo {
		p.Rules = append(p.Rules, policy.RequireTwo(fallbackApprovers(repo)))
	}
	if o.DefinitionAuthors <= 0 {
		return nil
	}
	sep := &p.Separation
	since, err := s.store.DefinitionsSince(ctx, db.DefinitionsSinceParams{Repository: repo.Name, Seconds: time.Duration(o.DefinitionAuthors).Seconds()})
	if err != nil {
		return err
	}
	sep.Since, sep.From, sep.Commit = since.Since.Time.UTC(), since.Commit, p.Commit
	commits, err := repo.Mirror.Authors(ctx, sep.From, p.Commit, files)
	if err != nil && sep.From != "" {
		// The commit Quai had then is no longer in the mirror (the history was rewritten): the
		// whole history counts.
		s.log.WarnContext(ctx, "the definitions commit the separation of duties starts from is gone; the whole history counts",
			slog.String("repository", repo.Name), slog.String("commit", sep.From), slog.Any("error", err))
		sep.From = ""
		commits, err = repo.Mirror.Authors(ctx, "", p.Commit, files)
	}
	if err != nil {
		return fmt.Errorf("who changed the definitions cannot be read: %w", err)
	}
	return s.matchAuthors(ctx, sep, commits)
}

// matchAuthors fills the separation's authors, unmatched identities and emails from the commits:
// an identity names the people whose verified email is its email, in any case, or, for Quai's
// own <name>@users.quai, the person of that name, whether they signed in yet or not.
func (s *Service) matchAuthors(ctx context.Context, sep *Separation, commits []gitx.Authored) error {
	var emails []string
	for _, c := range commits {
		for _, id := range c.Identities {
			if id.Email != "" {
				emails = append(emails, strings.ToLower(id.Email))
			}
		}
	}
	slices.Sort(emails)
	sep.Emails = slices.Compact(emails)
	users, err := s.store.UsersByEmail(ctx, sep.Emails)
	if err != nil {
		return err
	}
	type identity struct{ name, email string }
	people := map[string][]string{}      // a person's commits
	unmatched := map[identity][]string{} // an identity's commits
	for _, c := range commits {
		for _, id := range c.Identities {
			var matched []string
			for _, u := range users {
				if id.Email != "" && strings.EqualFold(u.Email, id.Email) {
					matched = append(matched, u.Name)
				}
			}
			if name, ok := quaiAddress(id.Email); ok {
				matched = append(matched, name)
			}
			for _, name := range matched {
				if !slices.Contains(people[name], c.SHA) {
					people[name] = append(people[name], c.SHA)
				}
			}
			if len(matched) == 0 {
				k := identity{id.Name, id.Email}
				if !slices.Contains(unmatched[k], c.SHA) {
					unmatched[k] = append(unmatched[k], c.SHA)
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(people)) {
		sep.Authors = append(sep.Authors, DefinitionAuthor{Name: name, Commits: people[name]})
	}
	for k, shas := range unmatched {
		sep.Unmatched = append(sep.Unmatched, UnmatchedIdentity{Name: k.name, Email: k.email, Commits: shas})
	}
	slices.SortFunc(sep.Unmatched, func(a, b UnmatchedIdentity) int { return strings.Compare(a.String(), b.String()) })
	return nil
}

// quaiAddress returns the person a <name>@users.quai address names: Quai's own address for a
// requester without a verified email (author).
func quaiAddress(email string) (string, bool) {
	at := strings.LastIndex(email, "@")
	if at <= 0 || !strings.EqualFold(email[at+1:], "users.quai") {
		return "", false
	}
	return email[:at], true
}

// separationDetails are the audit details of a request's separation of duties, with people and
// identities as pseudonyms; nil when the repository has no options.
func separationDetails(sep Separation) map[string]any {
	if sep.DefinitionAuthors <= 0 && sep.Destroys == "" {
		return nil
	}
	d := map[string]any{}
	if sep.Destroys != "" {
		d["destroys"] = sep.Destroys
	}
	if sep.DefinitionAuthors > 0 {
		excluded := []map[string]any{}
		for _, a := range sep.Authors {
			excluded = append(excluded, map[string]any{"person": audit.Personal(a.Name), "commits": a.Commits})
		}
		unmatched := []map[string]any{}
		for _, u := range sep.Unmatched {
			unmatched = append(unmatched, map[string]any{"identity": audit.Personal(u.String()), "commits": u.Commits})
		}
		d["definitionAuthors"], d["since"], d["from"], d["excluded"], d["unmatched"] = sep.DefinitionAuthors.String(), sep.Since, sep.From, excluded, unmatched
	}
	return d
}

// separationTightened says how the repository's separation of duties became stricter than a
// request's was planned with, or "": a longer definitionAuthors period, or requireTwo where the
// request had none. A looser option leaves the request as planned.
func (s *Service) separationTightened(r db.Request, repository string) string {
	repo, err := s.reg.Get(repository)
	if err != nil {
		return ""
	}
	now, was := repo.Document.Spec.Enforcement.SeparationOfDuties, SeparationOf(r)
	var why []string
	if now.DefinitionAuthors > was.DefinitionAuthors {
		why = append(why, "definition authors of the last "+now.DefinitionAuthors.String()+" cannot approve")
	}
	if now.Destroys == repositories.RequireTwo && was.Destroys != repositories.RequireTwo {
		why = append(why, "a plan that destroys something needs two approvers")
	}
	if len(why) == 0 {
		return ""
	}
	return "the repository's separation of duties became stricter since the plan (" + strings.Join(why, "; ") + ")"
}

// WhyNotReviewer says why someone a rule of a request pending approval names cannot review it,
// or "": they requested it, or they changed its definitions in the separation of duties' period
// before it.
func WhyNotReviewer(r db.Request, p authz.Principal) string {
	if r.State != PendingApproval || p.Name == "" || CanReview(r, p) {
		return ""
	}
	unseparated := contextOf(r)
	unseparated.Requester, unseparated.DefinitionAuthors, unseparated.DefinitionAuthorEmails = "", nil, nil
	if !eligible(r, p, unseparated) {
		return "" // no rule names them
	}
	if p.Name == r.Requester {
		return "You requested this change: someone else approves it."
	}
	sep := SeparationOf(r)
	if commits := sep.commitsOf(p.Name, p.Email); len(commits) > 0 {
		short := make([]string, len(commits))
		for i, c := range commits {
			short[i] = c[:min(len(c), 12)]
		}
		return fmt.Sprintf("You changed the definitions this request was planned with (%s) in the %s before it: the repository's separation of duties lets someone else approve it.",
			strings.Join(short, ", "), sep.DefinitionAuthors)
	}
	return ""
}
