package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/telemetry"
)

// Microsoft Graph at an Entra ID sign-in (docs/design/08-identity-access-tenancy.md
// #microsoft-entra-id-the-pilots-idp, open question 68): the person's groups when the ID token has
// too many to list them, and their manager. Quai calls it with the access token the sign-in itself
// returned, for the signed-in person only (/me), with delegated permissions: User.Read for the
// groups, User.Read.All for the manager. The token stays in memory for the sign-in; it is never
// stored, logged, or put in an error.
//
// Quai calls auth.graph.baseURL and nothing else: it ignores the endpoint an overage claim
// (_claim_sources) names, follows no redirect, and refuses an answer that points elsewhere
// (@odata.nextLink).

// Limits of a sign-in's Graph lookups.
const (
	// graphMaxGroups is the most groups Quai takes from Graph; a person in more cannot sign in.
	graphMaxGroups = 10000
	// graphMaxBody is the largest answer Quai reads: 10,000 group IDs take about 400 KB.
	graphMaxBody = 1 << 20
	// graphRetries is how many times a throttled (429) or unavailable (503, 504) call is retried.
	graphRetries = 2
	// graphMaxWait is the longest Retry-After Quai waits for; a longer one fails the lookup.
	graphMaxWait = 5 * time.Second
	// graphTimeout bounds all the Graph calls of one sign-in.
	graphTimeout = 20 * time.Second
)

// Where the manager came from.
const (
	ManagerFromClaim = "claim"
	ManagerFromGraph = "graph"
)

type graphClient struct {
	cfg    config.Graph
	client *http.Client
	sleep  func(context.Context, time.Duration) error
}

func newGraph(cfg *config.Graph, client *http.Client) *graphClient {
	if cfg == nil {
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	c := *client
	// A redirect is a URL from a response: never followed.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &graphClient{cfg: *cfg, client: &c, sleep: sleep}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// graphError is what Graph answered to a call that failed: the status and Graph's error code,
// never the token.
type graphError struct {
	status int
	code   string
}

func (e *graphError) Error() string {
	if e.code == "" {
		return fmt.Sprintf("Microsoft Graph answered %d", e.status)
	}
	return fmt.Sprintf("Microsoft Graph answered %d %s", e.status, e.code)
}

// call sends one request to Graph's base URL and decodes a 200's JSON into out; it retries a
// throttled or unavailable call, waiting as Retry-After says, within graphMaxWait. It returns the
// status of the last answer.
func (g *graphClient) call(ctx context.Context, token, method, path string, body []byte, out any) (int, error) {
	for attempt := 0; ; attempt++ {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, g.cfg.BaseURL+path, r)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := g.client.Do(req)
		if err != nil {
			return 0, err
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, graphMaxBody+1))
		_ = resp.Body.Close()
		if err != nil {
			return resp.StatusCode, err
		}
		if len(raw) > graphMaxBody {
			return resp.StatusCode, fmt.Errorf("the answer of Microsoft Graph is over %d bytes", graphMaxBody)
		}
		switch resp.StatusCode {
		case http.StatusOK:
			if err := json.Unmarshal(raw, out); err != nil {
				return resp.StatusCode, fmt.Errorf("the answer of Microsoft Graph: %w", err)
			}
			return resp.StatusCode, nil
		case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			wait := time.Duration(attempt+1) * time.Second
			if s := resp.Header.Get("Retry-After"); s != "" {
				n, err := strconv.Atoi(s)
				if err != nil || n < 0 {
					return resp.StatusCode, graphErr(resp.StatusCode, raw)
				}
				wait = time.Duration(n) * time.Second
			}
			if attempt >= graphRetries || wait > graphMaxWait {
				return resp.StatusCode, graphErr(resp.StatusCode, raw)
			}
			if err := g.sleep(ctx, wait); err != nil {
				return resp.StatusCode, err
			}
		default:
			return resp.StatusCode, graphErr(resp.StatusCode, raw)
		}
	}
}

// graphErr reads Graph's error code from an error answer
// (https://learn.microsoft.com/en-us/graph/errors): {"error": {"code": ..., "message": ...}}.
func graphErr(status int, raw []byte) error {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	code := e.Error.Code
	if len(code) > 100 || strings.ContainsFunc(code, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		code = ""
	}
	return &graphError{status: status, code: code}
}

// groups returns the object IDs of the security groups the signed-in person is a member of,
// directly or not: POST /v1.0/me/getMemberObjects with securityEnabledOnly, which Microsoft's ID
// token reference names for a group overage and which the delegated permission User.Read allows
// (https://learn.microsoft.com/en-us/graph/api/directoryobject-getmemberobjects?view=graph-rest-1.0).
// Its documentation describes no paging: an answer with @odata.nextLink is only part of the
// groups, and fails, as do more than graphMaxGroups groups and a value that is not an object ID.
func (g *graphClient) groups(ctx context.Context, token string) ([]string, error) {
	var out struct {
		Value    []string `json:"value"`
		NextLink string   `json:"@odata.nextLink"`
	}
	if _, err := g.call(ctx, token, http.MethodPost, "/v1.0/me/getMemberObjects", []byte(`{"securityEnabledOnly":true}`), &out); err != nil {
		return nil, err
	}
	switch {
	case out.NextLink != "":
		return nil, errors.New("the answer of Microsoft Graph has only part of the groups (@odata.nextLink), which Quai does not follow")
	case len(out.Value) > graphMaxGroups:
		return nil, fmt.Errorf("the person is in %d groups in Microsoft Graph, more than the %d Quai takes", len(out.Value), graphMaxGroups)
	}
	groups := make([]string, 0, len(out.Value))
	for _, id := range out.Value {
		if _, err := uuid.Parse(id); err != nil {
			return nil, errors.New("a group Microsoft Graph listed has an ID that is not an object ID")
		}
		groups = append(groups, strings.ToLower(id))
	}
	slices.Sort(groups)
	return slices.Compact(groups), nil
}

// manager returns the object ID of the signed-in person's manager: GET /v1.0/me/manager, which
// the delegated permission User.Read.All allows
// (https://learn.microsoft.com/en-us/graph/api/user-list-manager?view=graph-rest-1.0). Without
// one (404), or when the manager is not a user (an organizational contact, who cannot sign in to
// Quai), it returns no ID and why.
func (g *graphClient) manager(ctx context.Context, token string) (id, none string, err error) {
	var out struct {
		Type string `json:"@odata.type"`
		ID   string `json:"id"`
	}
	status, err := g.call(ctx, token, http.MethodGet, "/v1.0/me/manager", nil, &out)
	switch {
	case status == http.StatusNotFound:
		return "", "Microsoft Graph has no manager for them", nil
	case err != nil:
		return "", "", err
	case out.Type != "" && out.Type != "#microsoft.graph.user":
		return "", "their manager in Microsoft Graph is not a user who can sign in", nil
	}
	if _, err := uuid.Parse(out.ID); err != nil {
		return "", "", errors.New("the manager Microsoft Graph answered has an ID that is not an object ID")
	}
	return strings.ToLower(out.ID), "", nil
}

// fromGraph completes a sign-in from Microsoft Graph, with the access token it returned: the
// groups of a group overage, and the manager. Graph is called only when the ID token's tenant is
// auth.graph.tenantId. A group overage Graph cannot resolve refuses the sign-in: letting the person
// in without their groups would hide what they may and may not do. A manager lookup that fails
// leaves the person with no manager, and says why, until a sign-in finds one.
func (a *Authenticator) fromGraph(ctx context.Context, u *login, token string) error {
	g := a.graph
	if g == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, graphTimeout)
	defer cancel()
	ours := strings.EqualFold(u.tenant, g.cfg.TenantID)
	if u.overage {
		if !ours {
			telemetry.GraphLookup(ctx, "groups", "skipped")
			return refusal{"your account is in more groups than the ID token can list, and your directory is not the one " +
				"Quai reads groups from; ask an administrator"}
		}
		groups, err := g.groups(ctx, token)
		if err != nil {
			telemetry.GraphLookup(ctx, "groups", "error")
			a.log.WarnContext(ctx, "login: the groups of a group overage could not be read from Microsoft Graph", slog.String("user", u.Name), slog.Any("error", err))
			return refusal{"your account is in more groups than the ID token can list, and Quai could not read them from " +
				"Microsoft Graph; try again later, or ask an administrator"}
		}
		telemetry.GraphLookup(ctx, "groups", "ok")
		u.Groups = groups
	}
	if !g.cfg.Manager {
		return nil
	}
	u.ManagerSource = ManagerFromGraph
	if !ours {
		telemetry.GraphLookup(ctx, "manager", "skipped")
		u.ManagerReason = "their sign-in's directory (tid) is not the one Quai reads managers from"
		return nil
	}
	id, none, err := g.manager(ctx, token)
	switch {
	case err != nil:
		telemetry.GraphLookup(ctx, "manager", "error")
		a.log.WarnContext(ctx, "login: the manager could not be read from Microsoft Graph", slog.String("user", u.Name), slog.Any("error", err))
		u.ManagerReason = "Quai could not read their manager from Microsoft Graph at their last sign-in"
	case none != "":
		telemetry.GraphLookup(ctx, "manager", "none")
		u.ManagerReason = none
	default:
		telemetry.GraphLookup(ctx, "manager", "ok")
		u.ManagerID = id
	}
	return nil
}
