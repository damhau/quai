package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/damhau/quai/backend/internal/connectors"
)

// graphQLError is a refusal in a GraphQL answer: GitHub answers 200 and says what it refused in
// errors, such as "Pull request Pull request is in clean status".
type graphQLError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (e *graphQLError) Error() string { return "github: " + e.Message }

// graphql runs a query or a mutation of GitHub's GraphQL API, the only one with auto-merge and the
// merge queue, and decodes its data into out (nil: ignored). It goes through the same client as the
// REST calls: the installation's token, the shared transport and its rate limits. The first error
// GitHub names is returned: ErrNotFound and ErrRateLimited for those types, a *graphQLError for the
// others.
func (c *Connector) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	req, err := c.client.NewRequest(ctx, http.MethodPost, "graphql", map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	var answer struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	resp, err := c.client.Do(req, &answer)
	if err := check(resp, err); err != nil {
		return err
	}
	if len(answer.Errors) > 0 {
		e := answer.Errors[0]
		switch e.Type {
		case "NOT_FOUND":
			return fmt.Errorf("%w: %s", connectors.ErrNotFound, e.Message)
		case "RATE_LIMITED":
			return fmt.Errorf("%w: %s", connectors.ErrRateLimited, e.Message)
		}
		return &e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(answer.Data, out); err != nil {
		return fmt.Errorf("github: the GraphQL answer: %w", err)
	}
	return nil
}

// optional is a GraphQL variable that is null when empty.
func optional(s string) any {
	if s == "" {
		return nil
	}
	return s
}
