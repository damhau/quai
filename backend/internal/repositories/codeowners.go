package repositories

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/engine/definitions"
)

// codeownersTTL is how long the owners read from the platform are kept. A commit on the branch
// reads them again at once (CODEOWNERS may have changed); Azure DevOps' reviewer policies change
// without one, so they are read again after this long.
const codeownersTTL = time.Minute

// ErrOwnersUnavailable is returned when the owners of a branch's paths cannot be read from the
// platform, so who may approve or who owns a resource cannot be decided.
var ErrOwnersUnavailable = errors.New("the owners the platform names cannot be read")

// Codeowners is who the platform says owns the paths of the repository's branch, its CODEOWNERS
// file or Azure DevOps' "Automatically included reviewers" policies, matched to Quai
// (docs/design/08-identity-access-tenancy.md#directory): an owner the repository document maps
// (spec.codeowners.mapping) is what it maps to; otherwise an owner written as an email address,
// or a user whose email the platform gives, is that email; any other owner matches no one.
type Codeowners struct {
	Commit string // the branch's head when the owners were read
	rules  []connectors.OwnerRule
	who    map[string][]string // owner as the platform names it → Quai entries; none: no one
	at     bool                // the platform writes owners with a leading @ (a CODEOWNERS file)
	readAt time.Time
}

// Owners are the owners of some paths.
type Owners struct {
	// Entries are the owners as Quai matches them: user:<name>, group:<name>, email:<address>.
	Entries []string `json:"entries"`
	// Unmatched are the owners the platform names, as it writes them, that match no one.
	Unmatched []string `json:"unmatched"`
}

// Of returns the owners of paths: those of every rule that covers one of them, as the platform
// applies its rules (on GitHub, the last matching line, which may have no owner). Both lists are
// sorted, without duplicates, and never nil.
func (c *Codeowners) Of(paths ...string) Owners {
	out := Owners{Entries: []string{}, Unmatched: []string{}}
	if c == nil {
		return out
	}
	for _, r := range c.rules {
		if r.Match == nil || !slices.ContainsFunc(paths, func(p string) bool { return r.Match(strings.TrimPrefix(p, "/")) }) {
			continue
		}
		for _, o := range r.Owners {
			if e := c.who[o]; len(e) > 0 {
				out.Entries = append(out.Entries, e...)
			} else {
				out.Unmatched = append(out.Unmatched, c.written(o))
			}
		}
	}
	slices.Sort(out.Entries)
	slices.Sort(out.Unmatched)
	out.Entries, out.Unmatched = slices.Compact(out.Entries), slices.Compact(out.Unmatched)
	return out
}

// written is an owner as the platform writes it: @alice and @org/team in a CODEOWNERS file.
func (c *Codeowners) written(owner string) string {
	if c.at && !isEmail(owner) {
		return "@" + owner
	}
	return owner
}

// Codeowners returns the owners of the repository's branch at commit, its head: read from the
// platform, or kept from a read of the same commit within the last minute. A platform without
// owner rules (generic Git) names no one.
func (repo *Repo) Codeowners(ctx context.Context, commit string) (*Codeowners, error) {
	repo.ownersMu.Lock()
	defer repo.ownersMu.Unlock()
	if c := repo.owners; c != nil && c.Commit == commit && time.Since(c.readAt) < codeownersTTL {
		return c, nil
	}
	c, err := repo.readCodeowners(ctx, commit)
	if err != nil {
		return nil, fmt.Errorf("%w from %s: %w", ErrOwnersUnavailable, repo.Connector.Info().Platform, err)
	}
	repo.owners = c
	return c, nil
}

func (repo *Repo) readCodeowners(ctx context.Context, commit string) (*Codeowners, error) {
	c := &Codeowners{Commit: commit, who: map[string][]string{}, at: repo.Connector.Info().Has(connectors.CapCodeOwnersFile), readAt: time.Now()}
	own := repo.Connector.Owners()
	if own == nil {
		return c, nil
	}
	rules, err := own.Rules(ctx, repo.Branch)
	if err != nil {
		return nil, err
	}
	c.rules = rules
	mapping := map[string]string{}
	for k, v := range repo.Document.Spec.Codeowners.Mapping {
		mapping[ownerKey(k)] = v
	}
	var lookup []string
	for _, r := range rules {
		for _, o := range r.Owners {
			if _, done := c.who[o]; done || o == "" || slices.Contains(lookup, o) {
				continue
			}
			switch v, ok := mapping[ownerKey(o)]; {
			case ok:
				c.who[o] = []string{v}
			case isEmail(o):
				c.who[o] = []string{"email:" + strings.ToLower(o)}
			case strings.ContainsAny(o, `/\`):
				// A group (org/team, [Project]\Group): only the mapping matches it.
			default:
				lookup = append(lookup, o)
			}
		}
	}
	if len(lookup) == 0 {
		return c, nil
	}
	emails, err := own.Emails(ctx, lookup)
	if err != nil {
		return nil, err
	}
	for _, o := range lookup {
		for _, e := range emails[o] {
			if isEmail(e) {
				c.who[o] = append(c.who[o], "email:"+strings.ToLower(e))
			}
		}
	}
	return c, nil
}

// ownerKey is how an owner is compared with the mapping's: without case or a leading @.
func ownerKey(owner string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(owner), "@"))
}

// isEmail reports whether an owner is written as an email address (alice@acme.ch), not @alice.
func isEmail(owner string) bool {
	i := strings.Index(owner, "@")
	return i > 0 && i < len(owner)-1 && !strings.ContainsAny(owner, " \t/\\")
}

// OwnedByCodeowners reports whether some type's resources are owned through CODEOWNERS.
func OwnedByCodeowners(defs *definitions.Repository) bool {
	if defs == nil {
		return false
	}
	return slices.ContainsFunc(slices.Collect(maps.Values(defs.Types)), func(t *definitions.ResourceType) bool {
		return t.Spec.Ownership != nil && t.Spec.Ownership.Codeowners
	})
}
