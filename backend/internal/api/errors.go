package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/chatlink"
	"github.com/damhau/quai/backend/internal/engine/edit"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/engine/schema"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/requests"
)

// Problem maps an error from the domain to the problem details a handler returns: the one place
// where domain errors become HTTP statuses (docs/engineering/backend.md#conventions). Field
// errors become 422 with one detail per field, located in the request body under prefix, such
// as "body.values.cpu". A definition that cannot plan the request (an expression that fails, a
// blueprint resource its type rejects) is a 422 with its message, as is an edit a file editor
// refuses (a value defined in code, a YAML anchor), located at its field. Errors it does not know
// are 500s without their message, which may hold internal details. An action that needs a
// step-up the caller's authentication does not satisfy is a 403 of type auth.StepUpProblemType,
// for the UI to offer a new sign-in. A link of a chat card refused is a 403 of type
// ChatLinkProblemType, its detail saying why.
func Problem(err error, prefix string) huma.StatusError {
	var (
		ve *schema.ValidationError
		re *plan.ResourceError
		xe *expr.Error
		se *auth.StepUpError
		lr *chatlink.Refusal
	)
	switch {
	case errors.As(err, &se):
		return stepUpProblem(se)
	case errors.As(err, &lr):
		return &huma.ErrorModel{Type: ChatLinkProblemType, Title: http.StatusText(http.StatusForbidden), Status: http.StatusForbidden, Detail: lr.Error()}
	case errors.As(err, &re) && (errors.As(re.Err, &ve) || errors.As(re.Err, &xe)):
		// The fields are those of the resource's document, not of the form.
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.As(err, &xe):
		return huma.Error422UnprocessableEntity(err.Error())
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
	case errors.Is(err, requests.ErrConflict), errors.Is(err, repositories.ErrNotEnforced):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, repositories.ErrOwnersUnavailable):
		// Who may approve, or who owns the resource, is unknown until the platform answers.
		return huma.Error503ServiceUnavailable(err.Error())
	case errors.Is(err, requests.ErrNotApprover), errors.Is(err, requests.ErrNotAllowed):
		return huma.Error403Forbidden(err.Error())
	case errors.Is(err, plan.ErrExists), errors.Is(err, plan.ErrPin):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, plan.ErrNoChange):
		return huma.Error422UnprocessableEntity(err.Error())
	}
	if _, reason, ok := edit.Refused(err); ok {
		return huma.Error422UnprocessableEntity("Quai cannot make this change safely: " + reason)
	}
	return huma.NewError(http.StatusInternalServerError, "internal error")
}

// ChatLinkProblemType is the type of the problem of a link of a chat card refused.
const ChatLinkProblemType = "https://quai.dev/problems/chat-link-refused"

// stepUpProblem is the problem of an action refused for want of a step-up.
func stepUpProblem(e *auth.StepUpError) huma.StatusError {
	return &huma.ErrorModel{Type: auth.StepUpProblemType, Title: http.StatusText(http.StatusForbidden), Status: http.StatusForbidden, Detail: e.Error()}
}

// requireStepUp refuses an action that needs a step-up the caller's authentication does not
// satisfy now, with the problem the UI offers a new sign-in for; the refusal is in the audit log.
func requireStepUp(ctx context.Context, su *auth.StepUp, attempted string, obj audit.Object) error {
	id, _ := auth.FromContext(ctx)
	err := su.Require(ctx, id, attempted, obj)
	var se *auth.StepUpError
	if errors.As(err, &se) {
		return stepUpProblem(se)
	}
	return err
}
