package repositories

import (
	"context"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/connectors"
)

// trustConnector reads a fixed protection and fixed owners, as a platform's connector would.
type trustConnector struct {
	connectors.Connector
	platform   string
	protection connectors.Protection
	rules      []connectors.OwnerRule
	emails     map[string][]string // what Emails gives, by owner
}

func (c trustConnector) Info() connectors.Info         { return connectors.Info{Platform: c.platform} }
func (c trustConnector) Policies() connectors.Policies { return c }
func (c trustConnector) Owners() connectors.Owners     { return c }
func (c trustConnector) Protection(context.Context, string) (connectors.Protection, error) {
	return c.protection, nil
}
func (c trustConnector) Verify(context.Context, string, string) (connectors.Enforcement, error) {
	return connectors.Enforcement{}, nil
}
func (c trustConnector) Rules(context.Context, string) ([]connectors.OwnerRule, error) {
	return c.rules, nil
}
func (c trustConnector) Emails(_ context.Context, names []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, n := range names {
		if e := c.emails[n]; e != nil {
			out[n] = e
		}
	}
	return out, nil
}

// TestTrustCodeOwnerReview: on GitHub, CODEOWNERS covering the definitions is not enough; the
// branch must also require code owners' review. Other platforms are unchanged.
func TestTrustCodeOwnerReview(t *testing.T) {
	quai := []connectors.OwnerRule{{Pattern: "/.quai/", Owners: []string{"org/platform"}, Match: func(p string) bool { return strings.HasPrefix(p, ".quai/") }}}
	for _, c := range []struct {
		platform string
		review   bool
		want     string // the one problem; "" for none
	}{
		{"github", false, "the branch main does not require code owners' review: CODEOWNERS only requests reviews of .quai/"},
		{"github", true, ""},
		{"gitea", false, ""},
	} {
		d := doc(t, "apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: infra }\nspec: { connector: "+c.platform+", url: https://example.com/o/r }\n")
		d.ApplyDefaults()
		repo := &Repo{Name: "infra", Document: d, Connector: trustConnector{platform: c.platform,
			protection: connectors.Protection{Branch: "main", Protected: true, CodeOwnerReview: c.review}, rules: quai}}
		problems, err := repo.Trust(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if c.want == "" && len(problems) != 0 || c.want != "" && (len(problems) != 1 || !strings.Contains(problems[0], c.want)) {
			t.Errorf("%s, code owners' review %v: %q", c.platform, c.review, problems)
		}
	}
}
