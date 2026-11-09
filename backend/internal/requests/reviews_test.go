package requests

import (
	"slices"
	"testing"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
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
