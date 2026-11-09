// Package scim is Quai's SCIM 2.0 service provider (RFC 7643, RFC 7644) at /scim/v2, for the
// identity provider's provisioning: the requests Microsoft Entra ID's provisioning service sends,
// as Microsoft documents them (docs/design/08-identity-access-tenancy.md#scim-provisioning).
//
// It takes a SCIM token (auth.scim.tokens), and nothing else: never an API token, never a
// session, which auth.Middleware does not even load on its path. What it does is written in the
// audit log as the provisioning's, the token's name, never a person's. It keeps people current
// between their sign-ins: who they are (bound to the identity provider's immutable identifier,
// externalId), whether they are active, their manager, and the groups SCIM provisions, which take
// away at once what a person's ID token gave them and SCIM does not, sessions and API tokens
// included. A deactivation ends at once their sessions, API tokens, delegations, requests not
// delivered yet, and approvals not counted yet.
package scim

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/time/rate"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Prefix is where SCIM answers: the tenant URL the identity provider is given.
const Prefix = "/scim/v2"

// TokenPrefix starts every SCIM token (quai scim-token): one is never taken for an API token, or
// the reverse.
const TokenPrefix = "quai_scim_" // #nosec G101 -- a token's prefix, not a credential

// Schemas and media type (RFC 7643, RFC 7644).
const (
	schemaUser       = "urn:ietf:params:scim:schemas:core:2.0:User"
	schemaGroup      = "urn:ietf:params:scim:schemas:core:2.0:Group"
	schemaEnterprise = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"
	schemaList       = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	schemaError      = "urn:ietf:params:scim:api:messages:2.0:Error"
	schemaPatch      = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	schemaConfig     = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	schemaType       = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	schemaSchema     = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	mediaType        = "application/scim+json"
)

// Pages: the count when none is asked, and the most a page holds.
const (
	defaultCount = 100
	maxCount     = 200
)

// scimRate and scimBurst bound the requests taken, before any work: 50 a second, 100 at once;
// more are answered 429, which Microsoft Entra ID's provisioning retries later.
const (
	scimRate  = 50
	scimBurst = 100
)

// touchEvery is how often a token's last use is written: at most once a minute.
const touchEvery = time.Minute

// Handler answers /scim/v2.
type Handler struct {
	cfg      config.SCIM
	dir      *auth.Directory
	reserved map[string]bool // folded to lower case
	store    *store.Store
	secrets  *secrets.Store
	requests *requests.Service
	base     string
	log      *slog.Logger
	now      func() time.Time
	limit    *rate.Limiter

	mu      sync.Mutex
	touched map[string]time.Time
}

// New returns the SCIM service provider of cfg.SCIM; nil without one: /scim/ is then absent.
// requests is nil without repositories: a deactivation then has no request to end.
func New(cfg config.Auth, publicURL string, st *store.Store, sec *secrets.Store, reqs *requests.Service, log *slog.Logger) *Handler {
	if cfg.SCIM == nil || st == nil {
		return nil
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &Handler{cfg: *cfg.SCIM, dir: auth.NewDirectory(cfg), reserved: map[string]bool{}, store: st, secrets: sec, requests: reqs,
		base: strings.TrimSuffix(publicURL, "/") + Prefix, log: log, now: time.Now, limit: rate.NewLimiter(scimRate, scimBurst),
		touched: map[string]time.Time{}}
	for _, r := range cfg.SCIM.Reserved {
		h.reserved[strings.ToLower(r)] = true
	}
	for _, u := range cfg.Dev.Users { // never provisioned (docs/design/08-identity-access-tenancy.md#scim-provisioning)
		h.reserved[strings.ToLower(u.Name)] = true
	}
	return h
}

// UseLimit sets the rate limit, for tests.
func (h *Handler) UseLimit(r rate.Limit, burst int) { h.limit = rate.NewLimiter(r, burst) }

// UseClock sets the clock a token's last use is throttled by, for tests.
func (h *Handler) UseClock(now func() time.Time) { h.now = now }

// Error is a SCIM error (RFC 7644 §3.12): its detail is Quai's words, never an internal error or
// what the request sent but an attribute's name.
type Error struct {
	Status int
	Type   string // scimType, or ""
	Detail string
}

func (e *Error) Error() string { return e.Detail }

func fail(status int, typ, format string, args ...any) *Error {
	return &Error{Status: status, Type: typ, Detail: fmt.Sprintf(format, args...)}
}

func invalid(typ string, err error) *Error {
	return &Error{Status: http.StatusBadRequest, Type: typ, Detail: err.Error()}
}

var errNotFound = fail(http.StatusNotFound, "", "no such resource")

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.limit.Allow() {
		w.Header().Set("Retry-After", "1")
		h.write(w, http.StatusTooManyRequests, errorBody(fail(http.StatusTooManyRequests, "", "too many requests: try again in a moment")))
		return
	}
	if !auth.IsSCIM(r) {
		h.write(w, http.StatusNotFound, errorBody(errNotFound))
		return
	}
	name, err := h.authenticate(r)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="quai-scim"`)
		}
		h.respond(w, r, 0, nil, err)
		return
	}
	ip := auth.RemoteIP(r)
	ctx := audit.WithActor(audit.WithIP(r.Context(), ip), audit.Actor{Type: audit.Provisioning, ID: name, IP: ip})
	h.touch(ctx, name)
	status, body, err := h.route(ctx, r)
	h.respond(w, r, status, body, err)
}

// authenticate returns the name of the SCIM token the request carries: its SHA-256 compared in
// constant time with each token's, whose secrets hold them. Nothing else is taken: no session,
// no API token, no token sent twice.
func (h *Handler) authenticate(r *http.Request) (string, error) {
	unauthorized := fail(http.StatusUnauthorized, "", "a SCIM token is needed")
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", unauthorized
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || !strings.HasPrefix(token, TokenPrefix) || len(token) > 128 {
		h.log.WarnContext(r.Context(), "a SCIM request without a SCIM token was refused", slog.String("ip", auth.RemoteIP(r)))
		return "", unauthorized
	}
	sum := sha256.Sum256([]byte(token))
	sent := []byte(hex.EncodeToString(sum[:]))
	matched, unresolved := "", false
	for _, t := range h.cfg.Tokens {
		want, err := h.secrets.Resolve(r.Context(), t.SHA256.Ref)
		hash := strings.ToLower(strings.TrimSpace(want.Reveal()))
		if err == nil && !sha256Re.MatchString(hash) {
			err = errors.New("the secret does not hold a SHA-256 in hex")
		}
		if err != nil {
			unresolved = true
			h.log.ErrorContext(r.Context(), "a SCIM token's secret could not be read", slog.String("token", t.Name), slog.String("error", err.Error()))
			continue
		}
		if subtle.ConstantTimeCompare(sent, []byte(hash)) == 1 && matched == "" {
			matched = t.Name
		}
	}
	switch {
	case matched != "":
		return matched, nil
	case unresolved:
		return "", fail(http.StatusServiceUnavailable, "", "the SCIM tokens cannot be checked now")
	}
	h.log.WarnContext(r.Context(), "a SCIM request with an unknown token was refused", slog.String("ip", auth.RemoteIP(r)))
	return "", unauthorized
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// touch records a token's use, once a minute at most; a failure is logged, the request goes on.
func (h *Handler) touch(ctx context.Context, name string) {
	now := h.now()
	h.mu.Lock()
	last, ok := h.touched[name]
	if ok && now.Sub(last) < touchEvery {
		h.mu.Unlock()
		return
	}
	h.touched[name] = now
	h.mu.Unlock()
	if err := h.store.TouchSCIMToken(ctx, name); err != nil {
		h.log.WarnContext(ctx, "a SCIM token's last use was not recorded", slog.String("token", name), slog.Any("error", err))
	}
}

// route answers an authenticated request.
func (h *Handler) route(ctx context.Context, r *http.Request) (int, any, error) {
	rest, ok := strings.CutPrefix(r.URL.Path, Prefix+"/")
	if !ok {
		return 0, nil, errNotFound
	}
	segs := strings.Split(rest, "/")
	method := r.Method
	switch {
	case len(segs) == 1 && segs[0] == "Users":
		switch method {
		case http.MethodGet:
			return h.listUsers(ctx, r)
		case http.MethodPost:
			return h.withBody(ctx, r, h.createUser)
		}
	case len(segs) == 2 && segs[0] == "Users":
		switch method {
		case http.MethodGet:
			return h.getUser(ctx, segs[1])
		case http.MethodPatch:
			return h.withBody(ctx, r, func(ctx context.Context, body object) (int, any, error) { return h.patchUser(ctx, segs[1], body) })
		case http.MethodDelete:
			return h.deleteUser(ctx, segs[1])
		case http.MethodPut:
			return 0, nil, fail(http.StatusNotImplemented, "", "PUT is not supported: Quai takes PATCH")
		}
	case len(segs) == 1 && segs[0] == "Groups":
		switch method {
		case http.MethodGet:
			return h.listGroups(ctx, r)
		case http.MethodPost:
			return h.withBody(ctx, r, func(ctx context.Context, body object) (int, any, error) { return h.createGroup(ctx, r, body) })
		}
	case len(segs) == 2 && segs[0] == "Groups":
		switch method {
		case http.MethodGet:
			return h.getGroup(ctx, r, segs[1])
		case http.MethodPatch:
			return h.withBody(ctx, r, func(ctx context.Context, body object) (int, any, error) { return h.patchGroup(ctx, segs[1], body) })
		case http.MethodDelete:
			return h.deleteGroup(ctx, segs[1])
		case http.MethodPut:
			return 0, nil, fail(http.StatusNotImplemented, "", "PUT is not supported: Quai takes PATCH")
		}
	case len(segs) == 1 && (segs[0] == "ServiceProviderConfig" || segs[0] == "ResourceTypes" || segs[0] == "Schemas"):
		if method == http.MethodGet {
			return http.StatusOK, h.discovery(segs[0]), nil
		}
	case len(segs) == 1 && segs[0] == "Bulk":
		return 0, nil, fail(http.StatusNotImplemented, "", "bulk operations are not supported")
	default:
		return 0, nil, errNotFound
	}
	return 0, nil, fail(http.StatusMethodNotAllowed, "", "this method is not allowed on this resource")
}

// withBody reads a request's body, a JSON object of at most maxBody in SCIM's media type or
// JSON's, and hands it to next.
func (h *Handler) withBody(ctx context.Context, r *http.Request, next func(context.Context, object) (int, any, error)) (int, any, error) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mt != mediaType && mt != "application/json") {
		return 0, nil, fail(http.StatusUnsupportedMediaType, "", "the body must be %s or application/json", mediaType)
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return 0, nil, fail(http.StatusBadRequest, "invalidSyntax", "the body could not be read")
	}
	if len(raw) > maxBody {
		return 0, nil, fail(http.StatusRequestEntityTooLarge, "", "the body is over %d bytes", maxBody)
	}
	body, err := decodeObject(raw)
	if err != nil {
		return 0, nil, invalid("invalidSyntax", err)
	}
	return next(ctx, body)
}

// respond writes a response, or the SCIM error of err: an internal one logged, and answered
// without its detail.
func (h *Handler) respond(w http.ResponseWriter, r *http.Request, status int, body any, err error) {
	var e *Error
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
	case errors.As(err, &e):
		status, body = e.Status, errorBody(e)
	case errors.As(err, &pgErr) && pgErr.Code == "23505": // two requests raced to the same name
		status, body = http.StatusConflict, errorBody(fail(http.StatusConflict, "uniqueness", "the resource conflicts with another"))
	default:
		h.log.ErrorContext(r.Context(), "a SCIM request failed", slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("error", err))
		status, body = http.StatusInternalServerError, errorBody(fail(http.StatusInternalServerError, "", "internal error"))
	}
	h.write(w, status, body)
}

func (h *Handler) write(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func errorBody(e *Error) map[string]any {
	out := map[string]any{"schemas": []string{schemaError}, "status": strconv.Itoa(e.Status), "detail": e.Detail}
	if e.Type != "" {
		out["scimType"] = e.Type
	}
	return out
}

// page reads startIndex (1-based, 1 when absent or less) and count (defaultCount when absent, at
// most maxCount, 0 for none) as offset and limit.
func page(r *http.Request) (skip, take int32, err error) {
	start, count := 1, defaultCount
	q := r.URL.Query()
	if s := q.Get("startIndex"); s != "" {
		if start, err = strconv.Atoi(s); err != nil || start > 1<<30 {
			return 0, 0, fail(http.StatusBadRequest, "invalidValue", "startIndex must be a number")
		}
	}
	if s := q.Get("count"); s != "" {
		if count, err = strconv.Atoi(s); err != nil {
			return 0, 0, fail(http.StatusBadRequest, "invalidValue", "count must be a number")
		}
	}
	start = max(start, 1)
	count = min(max(count, 0), maxCount)
	return int32(start - 1), int32(count), nil // #nosec G109 G115 -- bounded above: start to 2^30, count to maxCount
}

// list is a ListResponse.
func list[T any](total int64, skip int32, resources []T) map[string]any {
	if resources == nil {
		resources = []T{}
	}
	return map[string]any{"schemas": []string{schemaList}, "totalResults": total, "startIndex": skip + 1,
		"itemsPerPage": len(resources), "Resources": resources}
}

// meta is a resource's meta.
type meta struct {
	ResourceType string    `json:"resourceType"`
	Created      time.Time `json:"created"`
	LastModified time.Time `json:"lastModified"`
	Location     string    `json:"location"`
}

// tx runs fn in a transaction that holds SCIM's lock: SCIM's changes do not interleave.
func (h *Handler) tx(ctx context.Context, fn func(pgx.Tx, *db.Queries) error) error {
	return h.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := q.LockSCIM(ctx); err != nil {
			return err
		}
		return fn(tx, q)
	})
}

// isReserved reports whether a name is out of SCIM's reach: auth.scim.reserved, or a development
// user's, in any case.
func (h *Handler) isReserved(name string) bool { return h.reserved[strings.ToLower(name)] }
