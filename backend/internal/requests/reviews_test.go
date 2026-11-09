package requests

import (
	"slices"
	"testing"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/store/db"
)

// TestToReviews: a review keeps the teams its reviewer had. One from before auth.teams kept only
// its groups: their teams are what auth.teams makes of them now, each group's name without it.
func TestToReviews(t *testing.T) {
	reviews := []db.Review{
		{Reviewer: "sam", Groups: []string{"acme-shop-ops"}, Teams: []string{"shop"}, Decision: "approve"},
		{Reviewer: "bob", Groups: []string{"shop", "acme-web"}, Decision: "approve"},
		{Reviewer: "zed", Groups: []string{}, Teams: []string{}, Decision: "deny"},
	}
	got := toReviews(reviews, &auth.Directory{})
	if !slices.Equal(got[0].Teams, []string{"shop"}) || !slices.Equal(got[1].Teams, []string{"acme-web", "shop"}) || len(got[2].Teams) != 0 {
		t.Errorf("reviews without auth.teams = %+v", got)
	}
	// With auth.teams, bob's group shop, which no entry maps, is not the team shop.
	got = toReviews(reviews, auth.NewDirectory(config.Auth{Teams: []config.TeamMapping{{Group: "acme-*", Team: "*"}}}))
	if !slices.Equal(got[0].Teams, []string{"shop"}) || !slices.Equal(got[1].Teams, []string{"web"}) || len(got[2].Teams) != 0 {
		t.Errorf("reviews with auth.teams = %+v", got)
	}
}

// TestCounted: the reviews that count are those of the change set reviewed; on a step-up request,
// only those whose reviewer had stepped up when reviewing, whatever their session became since.
func TestCounted(t *testing.T) {
	all := []db.Review{
		{Reviewer: "alice", ChangeSetHash: "h2", StepUp: true},
		{Reviewer: "bob", ChangeSetHash: "h2"},
		{Reviewer: "carol", ChangeSetHash: "h1", StepUp: true},
	}
	names := func(rs []db.Review) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Reviewer)
		}
		return out
	}
	for _, tt := range []struct {
		stepUp bool
		hash   string
		want   []string
	}{
		{false, "h2", []string{"alice", "bob"}},
		{true, "h2", []string{"alice"}},
		{true, "h1", []string{"carol"}},
		{false, "", []string{"alice", "bob", "carol"}},
		{true, "", []string{"alice", "carol"}},
	} {
		if got := names(counted(db.Request{StepUp: tt.stepUp}, all, tt.hash)); !slices.Equal(got, tt.want) {
			t.Errorf("step-up %v, change set %q: %v, want %v", tt.stepUp, tt.hash, got, tt.want)
		}
	}
}

// TestNeedsStepUp: an approval block asks for a step-up when its stepUp condition holds, and when
// it cannot be evaluated.
func TestNeedsStepUp(t *testing.T) {
	env, err := expr.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	cond := func(src string) definitions.Approval {
		p, err := env.CompileCondition(src)
		if err != nil {
			t.Fatal(err)
		}
		return definitions.Approval{StepUp: src, StepUpCondition: p}
	}
	prod := expr.Vars{Params: map[string]any{"env": "prod"}}
	dev := expr.Vars{Params: map[string]any{"env": "dev"}}
	for _, tt := range []struct {
		name string
		a    definitions.Approval
		v    expr.Vars
		want bool
	}{
		{"no stepUp", definitions.Approval{}, prod, false},
		{"stepUp true", cond("true"), dev, true},
		{"the condition holds", cond("params.env == 'prod'"), prod, true},
		{"the condition does not hold", cond("params.env == 'prod'"), dev, false},
		{"the condition fails", cond("inputs.missing > 3"), dev, true},
		{"not compiled", definitions.Approval{StepUp: "params.env == 'prod'"}, dev, true},
	} {
		if got := needsStepUp(tt.a, tt.v); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}
