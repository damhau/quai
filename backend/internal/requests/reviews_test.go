package requests

import (
	"slices"
	"testing"

	"github.com/damhau/quai/backend/internal/store/db"
)

// TestToReviews: a review keeps the teams its reviewer had; one from before auth.teams has none,
// and its groups were its teams.
func TestToReviews(t *testing.T) {
	got := toReviews([]db.Review{
		{Reviewer: "sam", Groups: []string{"acme-shop-ops"}, Teams: []string{"shop"}, Decision: "approve"},
		{Reviewer: "bob", Groups: []string{"shop"}, Decision: "approve"},
		{Reviewer: "zed", Groups: []string{}, Teams: []string{}, Decision: "deny"},
	})
	if !slices.Equal(got[0].Teams, []string{"shop"}) || !slices.Equal(got[1].Teams, []string{"shop"}) || len(got[2].Teams) != 0 {
		t.Errorf("reviews = %+v", got)
	}
}
