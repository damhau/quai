package api

import (
	"errors"
	"maps"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/engine/schema"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/requests"
)

// Problem maps an error from the domain to the problem details a handler returns: the one place
// where domain errors become HTTP statuses (docs/engineering/backend.md#conventions). Field
// errors become 422 with one detail per field, located in the request body under prefix, such
// as "body.document". Errors it does not know are 500s without their message, which may hold
// internal details.
func Problem(err error, prefix string) huma.StatusError {
	var ve *schema.ValidationError
	switch {
	case errors.As(err, &ve):
		details := make([]error, 0, len(ve.Fields))
		for _, f := range slices.Sorted(maps.Keys(ve.Fields)) {
			loc := prefix
			if f != "$" {
				loc = prefix + "." + f
			}
			details = append(details, &huma.ErrorDetail{Location: loc, Message: ve.Fields[f]})
		}
		return huma.Error422UnprocessableEntity("some fields need attention", details...)
	case errors.Is(err, plan.ErrNotFound), errors.Is(err, requests.ErrNotFound), errors.Is(err, repositories.ErrNotFound):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, requests.ErrConflict):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, requests.ErrNotApprover):
		return huma.Error403Forbidden(err.Error())
	case errors.Is(err, plan.ErrExists):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, plan.ErrNoChange):
		return huma.Error422UnprocessableEntity(err.Error())
	}
	return huma.NewError(http.StatusInternalServerError, "internal error")
}
