package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	gh "github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/connectors"
)

type owners Connector

// codeOwnersFiles are where GitHub reads CODEOWNERS from, in this order: the first found counts.
var codeOwnersFiles = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

// Rules reads the CODEOWNERS file of branch as GitHub applies it. GitHub ignores a line whose
// pattern it cannot read, and an owner it does not know or who lacks write access (a team
// included), so Rules leaves out what GitHub's check of the file reports. GitHub checks a new file
// after a moment, and answers 404 until then (measured): Rules then returns an error, rather than
// owners GitHub may ignore.
func (o *owners) Rules(ctx context.Context, branch string) ([]connectors.OwnerRule, error) {
	c := (*Connector)(o)
	for _, file := range codeOwnersFiles {
		fc, _, resp, err := c.client.Repositories.GetContents(ctx, c.owner, c.repo, file, &gh.RepositoryContentGetOptions{Ref: branch})
		if err := check(resp, err); errors.Is(err, connectors.ErrNotFound) || err == nil && fc == nil {
			continue // absent, or a directory
		} else if err != nil {
			return nil, err
		}
		src, err := fc.GetContent()
		if err != nil {
			return nil, fmt.Errorf("github: %s: %w", file, err)
		}
		lines := parseCodeOwners(src)
		found, resp, err := c.client.Repositories.GetCodeownersErrors(ctx, c.owner, c.repo, &gh.GetCodeownersErrorsOptions{Ref: branch})
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("github: GitHub has not checked %s on %s yet", file, branch)
		}
		if err := check(resp, err); err != nil {
			return nil, err
		}
		for _, e := range found.Errors {
			if e.Path == file {
				lines = drop(lines, e)
			}
		}
		return ownerRules(lines), nil
	}
	return nil, nil
}

// Emails returns the verified emails of users among names in the domains the organization has
// verified (GraphQL's organizationVerifiedDomainEmails, read with the Members permission): GitHub
// gives an app no other email of its users. An org/team owner is a team, an owner written as an
// email address needs no lookup, and the repository of a user, not an organization, has no verified
// domains: they are left out, as are users without such an email. A query reads up to 50 users.
func (o *owners) Emails(ctx context.Context, names []string) (map[string][]string, error) {
	c := (*Connector)(o)
	out := map[string][]string{}
	var logins []string
	for _, n := range names {
		if n != "" && !strings.ContainsAny(n, "/@") && !slices.Contains(logins, n) {
			logins = append(logins, n)
		}
	}
	for batch := range slices.Chunk(logins, 50) {
		more, err := c.verifiedEmails(ctx, batch)
		if err != nil {
			return nil, err
		}
		maps.Copy(out, more)
	}
	return out, nil
}

// verifiedEmails reads the verified-domain emails of logins in one query.
func (c *Connector) verifiedEmails(ctx context.Context, logins []string) (map[string][]string, error) {
	out := map[string][]string{}
	vars := map[string]any{"owner": c.owner}
	var q strings.Builder
	q.WriteString("query($owner: String!")
	for i := range logins {
		fmt.Fprintf(&q, ", $u%d: String!", i)
		vars[fmt.Sprintf("u%d", i)] = logins[i]
	}
	q.WriteString(") { repositoryOwner(login: $owner) { __typename }")
	for i := range logins {
		fmt.Fprintf(&q, " u%d: user(login: $u%d) { organizationVerifiedDomainEmails(login: $owner) }", i, i)
	}
	q.WriteString(" }")
	data, errs, err := c.graphqlData(ctx, q.String(), vars)
	if err != nil {
		return nil, err
	}
	var answer map[string]json.RawMessage
	_ = json.Unmarshal(data, &answer)
	var owner struct {
		Typename string `json:"__typename"`
	}
	if raw := answer["repositoryOwner"]; raw != nil {
		_ = json.Unmarshal(raw, &owner)
	}
	if owner.Typename == "User" {
		return out, nil // a user's repository: no organization, no verified domains
	}
	// A user not found leaves its part null; any other refusal fails the lookup.
	for _, e := range errs {
		if e.Type == "RATE_LIMITED" {
			return nil, fmt.Errorf("%w: %s", connectors.ErrRateLimited, e.Message)
		}
		if e.Type != "NOT_FOUND" {
			return nil, &e
		}
	}
	if owner.Typename != "Organization" {
		return out, nil
	}
	for i, login := range logins {
		var u struct {
			Emails []string `json:"organizationVerifiedDomainEmails"`
		}
		if raw := answer[fmt.Sprintf("u%d", i)]; raw != nil && json.Unmarshal(raw, &u) == nil && len(u.Emails) > 0 {
			out[login] = u.Emails
		}
	}
	return out, nil
}

// ownerLine is a rule of a CODEOWNERS file.
type ownerLine struct {
	number  int // from 1
	pattern string
	re      *regexp.Regexp // nil when GitHub cannot read the pattern
	owners  []ownerToken
	others  []int // the columns of the words that are not owners
}

type ownerToken struct {
	name   string // without the @: a user, org/team, or an email address
	column int    // from 1, where it starts on its line
}

// parseCodeOwners reads CODEOWNERS lines: a pattern, then owners (@user, @org/team or an email
// address), up to a # comment. A backslash escapes the next character, a space included.
func parseCodeOwners(src string) []ownerLine {
	var out []ownerLine
	for i, line := range strings.Split(src, "\n") {
		fields := tokens(strings.TrimSuffix(line, "\r"))
		if len(fields) == 0 || strings.HasPrefix(fields[0].name, "#") {
			continue
		}
		l := ownerLine{number: i + 1, pattern: fields[0].name, re: codeOwnersPattern(fields[0].name)}
		for _, f := range fields[1:] {
			if strings.HasPrefix(f.name, "#") {
				break
			}
			switch name, ok := strings.CutPrefix(f.name, "@"); {
			case ok && name != "":
				l.owners = append(l.owners, ownerToken{name: name, column: f.column})
			case !ok && strings.Contains(f.name, "@"):
				l.owners = append(l.owners, f)
			default:
				l.others = append(l.others, f.column)
			}
		}
		out = append(out, l)
	}
	return out
}

// tokens splits a line on unescaped blanks, keeping each token's escapes and column.
func tokens(line string) []ownerToken {
	var out []ownerToken
	start := -1
	for i := 0; i < len(line); i++ {
		switch b := line[i]; {
		case b == ' ' || b == '\t':
			if start >= 0 {
				out = append(out, ownerToken{name: line[start:i], column: start + 1})
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
			if b == '\\' {
				i++
			}
		}
	}
	if start >= 0 {
		out = append(out, ownerToken{name: line[start:], column: start + 1})
	}
	return out
}

// codeOwnersPattern compiles a pattern as GitHub reads it: gitignore-like and case-sensitive. A
// leading slash, or one in the middle, anchors it at the root; otherwise it matches at any depth.
// It covers what is under a folder it matches, except when it ends in a lone * (docs/* is the
// files of docs/ only), and a trailing slash matches folders only. ** crosses folders, * and ?
// do not. It returns nil for what GitHub refuses: an unescaped [ or ], and a ! negation, which
// CODEOWNERS does not have.
func codeOwnersPattern(pattern string) *regexp.Regexp {
	if pattern == "" || strings.HasPrefix(pattern, "!") {
		return nil
	}
	p := strings.TrimPrefix(pattern, "/")
	anchored := p != pattern
	dir := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return nil
	}
	segments := strings.Split(p, "/")
	anchored = anchored || len(segments) > 1
	var re strings.Builder
	re.WriteString(`\A`)
	if !anchored {
		re.WriteString(`(?:.*/)?`)
	}
	last := len(segments) - 1
	for i, seg := range segments {
		if seg == "**" {
			if i == last {
				re.WriteString(`.*`)
			} else {
				re.WriteString(`(?:.*/)?`)
			}
			continue
		}
		part, ok := glob(seg)
		if !ok {
			return nil
		}
		re.WriteString(part)
		if i < last {
			re.WriteString("/")
		}
	}
	switch {
	case dir:
		re.WriteString(`/.*`)
	case segments[last] == "*", segments[last] == "**":
	default:
		re.WriteString(`(?:/.*)?`)
	}
	re.WriteString(`\z`)
	return regexp.MustCompile(re.String())
}

// glob translates one path segment: * and ? within it, \x as x; an unescaped [ or ] is refused.
func glob(seg string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		switch c := seg[i]; c {
		case '*':
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		case '[', ']':
			return "", false
		case '\\':
			if i+1 < len(seg) {
				i++
				b.WriteString(regexp.QuoteMeta(seg[i : i+1]))
			}
		default:
			b.WriteString(regexp.QuoteMeta(seg[i : i+1]))
		}
	}
	return b.String(), true
}

// drop applies an error GitHub found in the file: a pattern it cannot read leaves the line out, an
// owner it does not know leaves that owner out. Any other error leaves the line without owners, so
// it covers nothing and still overrides the lines before it.
func drop(lines []ownerLine, e *gh.CodeownersError) []ownerLine {
	for i := range lines {
		l := &lines[i]
		if l.number != e.Line {
			continue
		}
		switch e.Kind {
		case "Invalid pattern":
			l.re = nil
		case "Unknown owner":
			kept := l.owners[:0]
			for _, o := range l.owners {
				if o.column != e.Column {
					kept = append(kept, o)
				}
			}
			if len(kept) == len(l.owners) && !slices.Contains(l.others, e.Column) {
				kept = nil // nothing starts where GitHub says: trust none of the line's owners
			}
			l.owners = kept
		default:
			l.owners = nil
		}
	}
	return lines
}

// ownerRules returns the lines GitHub applies as rules. GitHub takes the last line matching a path,
// so a rule's Match reports a path only when no later line matches it.
func ownerRules(lines []ownerLine) []connectors.OwnerRule {
	var valid []ownerLine
	for _, l := range lines {
		if l.re != nil {
			valid = append(valid, l)
		}
	}
	var out []connectors.OwnerRule
	for i, l := range valid {
		rule := connectors.OwnerRule{Pattern: l.pattern}
		for _, o := range l.owners {
			rule.Owners = append(rule.Owners, o.name)
		}
		later := valid[i+1:]
		rule.Match = func(path string) bool {
			path = strings.TrimPrefix(path, "/")
			if !l.re.MatchString(path) {
				return false
			}
			for _, n := range later {
				if n.re.MatchString(path) {
					return false
				}
			}
			return true
		}
		out = append(out, rule)
	}
	return out
}
