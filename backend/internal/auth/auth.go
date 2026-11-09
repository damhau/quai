// Package auth establishes who makes each request (docs/design/08-identity-access-tenancy.md
// #authentication): a browser session after an OpenID Connect login (authorization code with
// PKCE), an API token, or, in development only, a user picked in the switcher. It puts the
// resulting identity in the request's context; internal/authz decides what that identity may do.
//
// Sessions live in PostgreSQL (scs); the browser holds only an HttpOnly, SameSite=Lax cookie,
// Secure in production. A request that changes state with a session must carry the session's
// CSRF token in the X-CSRF-Token header. API tokens are random, shown once, stored as SHA-256
// hashes, scoped and expiring.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Identity is who makes a request.
type Identity struct {
	Principal authz.Principal
	Email     string
	// CSRFToken is the session's token, for the client to send back; empty with an API token.
	CSRFToken string
	// TokenID is the API token used, if any.
	TokenID string
	// AuthTime is when the person last authenticated with the identity provider: the ID token's
	// auth_time, zero when it had none (and for an API token); the development switcher's login.
	AuthTime time.Time
	// ACR and ACRS are the ID token's acr and Entra ID's acrs (authentication contexts): how the
	// person authenticated, which step-ups check.
	ACR  string
	ACRS []string
}

type ctxKey struct{}

// FromContext returns the identity of the request, if anyone is signed in.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok && id.Principal.Name != ""
}

// WithIdentity returns ctx with an identity, for tests and jobs acting for someone.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// sessionUser is what a session remembers about its user. AuthTime is under auth_time since
// P4-06: sessions from before, whose time could be the login's rather than the ID token's, have
// none, and must sign in again to step up.
type sessionUser struct {
	Name     string    `json:"name"`
	Email    string    `json:"email,omitempty"`
	Groups   []string  `json:"groups"`
	AuthTime time.Time `json:"auth_time"`
	ACR      string    `json:"acr,omitempty"`
	ACRS     []string  `json:"acrs,omitempty"`
}

// login is a sign-in: the session's user, how they signed in, and the identity provider's
// identifiers for them (none for a development user).
type login struct {
	sessionUser
	Method   string
	Subject  string // the ID token's sub
	ObjectID string // Entra ID's oid
	StepUp   bool   // a sign-in again, to step up
	// The manager the sign-in found: the identity provider's immutable identifier of them, where it
	// comes from (ManagerFromClaim, ManagerFromGraph; empty when nothing is configured), and why there
	// is none.
	ManagerID, ManagerSource, ManagerReason string

	overage bool   // the ID token has too many groups to list them
	tenant  string // Entra ID's tid
}

// WebhooksPath is where Git platforms deliver webhooks: no sessions or tokens there, the
// repository's connector verifies each delivery.
const WebhooksPath = "/webhooks/"

// RunsPath is where runners report runs; its Authorization header carries a run token, not an
// API token (docs/design/06-runs-status-and-drift.md#run-report-protocol).
const RunsPath = "/api/v1/runs"

// FluxRunsPath is where Flux's notification controller reports, with a run token too.
const FluxRunsPath = "/api/v1/runs/flux"

// IsRunsPath reports whether path is one of the run report endpoints, which runners call with
// a run token.
func IsRunsPath(path string) bool { return path == RunsPath || path == FluxRunsPath }

// SlackInteractionsPath is where Slack sends the clicks on the buttons of Quai's messages: no
// session is loaded or saved there, no token or CSRF token checked; the signing secret verifies
// each request (notify.SlackInteractions).
const SlackInteractionsPath = "/slack/interactions"

// IsSlackInteractions reports whether a request is for SlackInteractionsPath, exactly: not a path
// under it, nor one that only decodes to it.
func IsSlackInteractions(r *http.Request) bool {
	return r.URL.Path == SlackInteractionsPath && r.URL.RawPath == ""
}

// Session keys.
const (
	keyUser     = "user"
	keyCSRF     = "csrf"
	keyState    = "oidc_state"
	keyNonce    = "oidc_nonce"
	keyVerifier = "oidc_verifier"
	keyNext     = "oidc_next"
	keyStepUp   = "oidc_step_up"
)

// Authenticator signs people in and identifies requests.
type Authenticator struct {
	cfg      config.Auth
	dir      *Directory
	sessions *scs.SessionManager
	store    *store.Store
	client   *http.Client // reaches the identity provider; nil: the default
	log      *slog.Logger
	stepUp   *StepUp
	graph    *graphClient // nil: Quai does not call Microsoft Graph
	now      func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider
}

// New returns an authenticator keeping sessions in s's database. It reaches the identity provider
// with client (nil: the default client).
func New(cfg *config.Config, s *store.Store, client *http.Client, log *slog.Logger) *Authenticator {
	sm := scs.New()
	// Expired sessions are deleted by a periodic job, not by a goroutine per process.
	sm.Store = pgxstore.NewWithCleanupInterval(s.Pool, 0)
	sm.Lifetime = cfg.Auth.Session.Lifetime
	sm.IdleTimeout = cfg.Auth.Session.IdleTimeout
	sm.Cookie.Name = "quai_session"
	sm.Cookie.HttpOnly = true
	sm.Cookie.SameSite = http.SameSiteLaxMode
	sm.Cookie.Secure = cfg.Env == config.Production
	sm.Cookie.Path = "/"
	return &Authenticator{cfg: cfg.Auth, dir: NewDirectory(cfg.Auth), sessions: sm, store: s, client: client, log: log,
		stepUp: NewStepUp(cfg.Auth, s), graph: newGraph(cfg.Auth.Graph, client), now: time.Now}
}

// UseSleep sets how Microsoft Graph lookups wait before they retry, for tests.
func (a *Authenticator) UseSleep(sleep func(context.Context, time.Duration) error) {
	if a.graph != nil {
		a.graph.sleep = sleep
	}
}

// UseClock sets the clock of step-ups and development logins, for tests.
func (a *Authenticator) UseClock(now func() time.Time) {
	a.now = now
	a.stepUp.UseClock(now)
}

// StepUp returns what decides step-ups.
func (a *Authenticator) StepUp() *StepUp { return a.stepUp }

// Mode returns how people sign in: oidc or dev.
func (a *Authenticator) Mode() string { return a.cfg.Mode }

// Directory returns what people's groups make them: their teams and grants.
func (a *Authenticator) Directory() *Directory { return a.dir }

// Middleware loads the session and identifies the request: an API token in the Authorization
// header, or else the session's user. It refuses a state-changing request made with a session
// but without its CSRF token. Slack's interactions (SlackInteractionsPath) pass through untouched:
// no session, no identity.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	identify := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if strings.HasPrefix(r.URL.Path, WebhooksPath) {
			next.ServeHTTP(w, r)
			return
		}
		ctx = audit.WithIP(ctx, RemoteIP(r))
		// Runners report with their repository's run token, which the run endpoint checks.
		if h := r.Header.Get("Authorization"); h != "" && !IsRunsPath(r.URL.Path) {
			id, err := a.tokenIdentity(ctx, h)
			if err != nil {
				writeProblem(w, http.StatusUnauthorized, err.Error())
				return
			}
			next.ServeHTTP(w, r.WithContext(audit.WithActor(WithIdentity(ctx, id), ActorOf(ctx, id))))
			return
		}
		id, ok := a.sessionIdentity(ctx)
		if ok && !safeMethod(r.Method) {
			sent := r.Header.Get("X-CSRF-Token")
			if subtle.ConstantTimeCompare([]byte(sent), []byte(id.CSRFToken)) != 1 {
				writeProblem(w, http.StatusForbidden, "missing or wrong X-CSRF-Token header")
				return
			}
		}
		if ok {
			ctx = audit.WithActor(WithIdentity(ctx, id), ActorOf(ctx, id))
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
	sessions := a.sessions.LoadAndSave(identify)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsSlackInteractions(r) {
			next.ServeHTTP(w, r)
			return
		}
		sessions.ServeHTTP(w, r)
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (a *Authenticator) sessionIdentity(ctx context.Context) (Identity, bool) {
	raw := a.sessions.GetBytes(ctx, keyUser)
	if raw == nil {
		return Identity{}, false
	}
	var u sessionUser
	if err := json.Unmarshal(raw, &u); err != nil || u.Name == "" {
		return Identity{}, false
	}
	p := a.principal(u.Name, u.Groups, nil)
	p.Email = u.Email
	return Identity{
		Principal: p,
		Email:     u.Email,
		CSRFToken: a.sessions.GetString(ctx, keyCSRF),
		AuthTime:  u.AuthTime,
		ACR:       u.ACR,
		ACRS:      u.ACRS,
	}, true
}

// principal gives a person the teams and grants their groups have.
func (a *Authenticator) principal(name string, groups, scopes []string) authz.Principal {
	p := authz.Principal{Name: name, Groups: groups, Teams: a.dir.Teams(groups), Scopes: scopes}
	for _, g := range a.dir.Grants(groups) {
		if !slices.Contains(p.Grants, g.Grant) {
			p.Grants = append(p.Grants, g.Grant)
		}
	}
	return p
}

// signIn starts a session for a user: a new session token, against fixation, and a CSRF token.
func (a *Authenticator) signIn(ctx context.Context, u sessionUser) error {
	if err := a.sessions.RenewToken(ctx); err != nil {
		return err
	}
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	a.sessions.Put(ctx, keyUser, raw)
	a.sessions.Put(ctx, keyCSRF, random(32))
	return nil
}

// Routes returns the /auth endpoints: login and callback for OIDC, the switcher in development,
// and logout.
func (a *Authenticator) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/logout", a.logout)
	switch a.cfg.Mode {
	case config.AuthOIDC:
		r.Get("/login", a.login)
		r.Get("/callback", a.callback)
	case config.AuthDev:
		r.Get("/dev/users", a.devUsers)
		r.Post("/dev/login", a.devLogin)
	}
	return r
}

func (a *Authenticator) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var recorded error
	if id, ok := FromContext(ctx); ok && id.TokenID == "" {
		recorded = a.store.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			return audit.Add(ctx, tx, audit.Event{Action: "auth.logout", Object: audit.Object{Type: "user", ID: id.Principal.Name}})
		})
	}
	// The session ends even when the audit log cannot record it.
	if err := errors.Join(a.sessions.Destroy(ctx), recorded); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// idpContext makes the identity provider's discovery, keys and token exchange use a's client.
func (a *Authenticator) idpContext(ctx context.Context) context.Context {
	if a.client == nil {
		return ctx
	}
	return oidc.ClientContext(ctx, a.client)
}

func (a *Authenticator) oauth(ctx context.Context) (*oidc.Provider, *oauth2.Config, error) {
	ctx = a.idpContext(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider == nil {
		p, err := oidc.NewProvider(ctx, a.cfg.OIDC.Issuer)
		if err != nil {
			return nil, nil, fmt.Errorf("identity provider %s: %w", a.cfg.OIDC.Issuer, err)
		}
		a.provider = p
	}
	return a.provider, &oauth2.Config{
		ClientID:     a.cfg.OIDC.ClientID,
		ClientSecret: a.cfg.OIDC.ClientSecret,
		RedirectURL:  a.cfg.OIDC.RedirectURL,
		Endpoint:     a.provider.Endpoint(),
		Scopes:       a.scopes(),
	}, nil
}

// scopes are those a login asks for: auth.oidc.scopes, and the Microsoft Graph permissions that
// auth.graph needs, which make the access token the login returns one for Graph.
func (a *Authenticator) scopes() []string {
	out := slices.Clone(a.cfg.OIDC.Scopes)
	for _, s := range a.cfg.Graph.GraphScopes() {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// login sends the browser to the identity provider, remembering what the callback must check and
// where to go next, a path on this server. With stepUp=1, the person authenticates again for a
// step-up (StepUp.stepUpOptions), then comes back to next: the decision they were making.
func (a *Authenticator) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, oc, err := a.oauth(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	state, nonce, verifier := random(24), random(24), oauth2.GenerateVerifier()
	a.sessions.Put(ctx, keyState, state)
	a.sessions.Put(ctx, keyNonce, nonce)
	a.sessions.Put(ctx, keyVerifier, verifier)
	a.sessions.Put(ctx, keyNext, localPath(q.Get("next")))
	opts := []oauth2.AuthCodeOption{oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)}
	if q.Get("stepUp") == "1" {
		a.sessions.Put(ctx, keyStepUp, true)
		opts = append(opts, a.stepUp.stepUpOptions()...)
	} else {
		a.sessions.Remove(ctx, keyStepUp)
	}
	http.Redirect(w, r, oc.AuthCodeURL(state, opts...), http.StatusFound)
}

// callback finishes the login: it checks the state, exchanges the code with the PKCE verifier,
// verifies the ID token and its nonce, and signs the user in.
func (a *Authenticator) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	state := a.sessions.PopString(ctx, keyState)
	nonce := a.sessions.PopString(ctx, keyNonce)
	verifier := a.sessions.PopString(ctx, keyVerifier)
	next := a.sessions.PopString(ctx, keyNext)
	stepUp := a.sessions.PopBool(ctx, keyStepUp)
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		writeProblem(w, http.StatusUnauthorized, "the identity provider refused the login: "+e)
		return
	}
	if state == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
		writeProblem(w, http.StatusBadRequest, "the login expired or was not started here; start again")
		return
	}
	provider, oc, err := a.oauth(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	tok, err := oc.Exchange(a.idpContext(ctx), q.Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		a.log.WarnContext(ctx, "login: code exchange failed", slog.Any("error", err))
		writeProblem(w, http.StatusUnauthorized, "the identity provider did not accept the login")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := provider.Verifier(&oidc.Config{ClientID: a.cfg.OIDC.ClientID}).Verify(a.idpContext(ctx), raw)
	if err != nil {
		a.log.WarnContext(ctx, "login: invalid ID token", slog.Any("error", err))
		writeProblem(w, http.StatusUnauthorized, "the identity provider returned an invalid ID token")
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		writeProblem(w, http.StatusUnauthorized, "the ID token does not belong to this login")
		return
	}
	u, err := a.userFromClaims(idt)
	if err != nil {
		a.log.WarnContext(ctx, "login refused", slog.Any("error", err))
		writeProblem(w, http.StatusForbidden, err.Error())
		return
	}
	// The access token is the sign-in's, for Graph's lookups now, and is dropped with tok.
	if err := a.fromGraph(ctx, &u, tok.AccessToken); err != nil {
		a.refuseOrFail(w, r, err)
		return
	}
	u.StepUp = stepUp
	if stepUp {
		// The person is signed in either way; the action they come back to says why it still
		// needs a step-up, and refuses it.
		if why := a.stepUp.Check(Identity{Principal: authz.Principal{Name: u.Name}, AuthTime: u.AuthTime, ACR: u.ACR, ACRS: u.ACRS}); why != "" {
			a.log.WarnContext(ctx, "login: the step-up is not satisfied", slog.String("user", u.Name), slog.String("reason", why))
		}
	}
	if err := a.recordLogin(ctx, u); err != nil {
		a.refuseOrFail(w, r, err)
		return
	}
	if err := a.signIn(ctx, u.sessionUser); err != nil {
		a.fail(w, r, err)
		return
	}
	a.log.InfoContext(ctx, "login", slog.String("user", u.Name), slog.Int("groups", len(u.Groups)))
	http.Redirect(w, r, next, http.StatusFound)
}

// verifiedEmail returns the ID token's email when the identity provider vouches for it: OIDC's
// email_verified, or Entra ID's optional claim xms_edov (the email's domain is verified by its
// owner), is the JSON boolean true. Otherwise the person has no email: owners named by email, and
// email: approvers, would match whoever could set the address (an Entra guest's home tenant).
func verifiedEmail(claims map[string]any) string {
	email, _ := claims["email"].(string)
	for _, c := range []string{"email_verified", "xms_edov"} {
		if v, ok := claims[c].(bool); ok && v {
			return email
		}
	}
	return ""
}

func (a *Authenticator) userFromClaims(idt *oidc.IDToken) (login, error) {
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return login{}, err
	}
	u := login{sessionUser: sessionUser{Email: verifiedEmail(claims)}, Method: config.AuthOIDC, Subject: idt.Subject}
	u.ObjectID, _ = claims["oid"].(string)
	u.tenant, _ = claims["tid"].(string)
	// A group overage: Entra ID names, in _claim_names, a source for the groups claim, whose
	// endpoint (_claim_sources) Quai ignores; or, in the implicit flow, says hasgroups. Only
	// Microsoft Graph, at auth.graph.baseURL, can then list the groups.
	if names, ok := claims["_claim_names"].(map[string]any); ok && names[a.cfg.OIDC.GroupsClaim] != nil {
		u.overage = true
	}
	if has, _ := claims["hasgroups"].(bool); has && a.cfg.OIDC.GroupsClaim == "groups" {
		u.overage = true
	}
	if u.overage && (a.graph == nil || !a.graph.cfg.Groups) {
		return login{}, errors.New("your account is in more groups than the ID token can list; " +
			"ask an administrator to emit only the groups assigned to Quai, to use app roles, or to let Quai read groups from Microsoft Graph")
	}
	if c := a.cfg.OIDC.ManagerClaim; c != "" {
		u.ManagerSource = ManagerFromClaim
		switch v := claims[c].(type) {
		case nil:
			u.ManagerReason = "their ID token has no " + c + " claim"
		case string:
			if v == "" || len(v) > 256 || strings.ContainsFunc(v, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
				u.ManagerReason = "the " + c + " claim of their ID token is not an identifier"
			} else {
				u.ManagerID = v
			}
		default:
			u.ManagerReason = "the " + c + " claim of their ID token is not a single identifier"
		}
	}
	names := slices.DeleteFunc(slices.Compact([]string{a.cfg.OIDC.NameClaim, "preferred_username"}), func(c string) bool { return c == "" })
	for _, c := range names {
		if s, _ := claims[c].(string); s != "" {
			u.Name = s
			break
		}
	}
	if u.Name == "" {
		// The email names the person only when the IdP vouches for it, as for matching owners.
		if u.Email == "" {
			why := "nor an email claim"
			if e, _ := claims["email"].(string); e != "" {
				why = "and its email claim is not verified (neither email_verified nor xms_edov is true)"
			}
			return login{}, fmt.Errorf("the identity provider sent no %s claim to name you by, %s; "+
				"ask an administrator to have it send one of these claims", strings.Join(names, " or "), why)
		}
		u.Name = u.Email
	}
	switch g := claims[a.cfg.OIDC.GroupsClaim].(type) {
	case []any:
		for _, v := range g {
			if s, ok := v.(string); ok {
				u.Groups = append(u.Groups, s)
			}
		}
	case string:
		u.Groups = []string{g}
	}
	// Only the identity provider says when and how the person authenticated: without auth_time,
	// the session has no authentication time, and no step-up is satisfied.
	if at, ok := claims["auth_time"].(float64); ok && at > 0 && at < 1<<40 {
		u.AuthTime = time.Unix(int64(at), 0)
	}
	u.ACR, _ = claims["acr"].(string)
	switch v := claims["acrs"].(type) {
	case []any:
		for _, c := range v {
			if s, ok := c.(string); ok {
				u.ACRS = append(u.ACRS, s)
			}
		}
	case string:
		u.ACRS = []string{v}
	}
	return u, nil
}

func (a *Authenticator) devUsers(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a.cfg.Dev.Users)
}

// devLogin signs in a user of the development switcher. Development only: configuration refuses
// the dev mode in production.
func (a *Authenticator) devLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeProblem(w, http.StatusBadRequest, "send {\"name\": \"<user>\"}")
		return
	}
	i := slices.IndexFunc(a.cfg.Dev.Users, func(u config.DevUser) bool { return u.Name == body.Name })
	if i < 0 {
		writeProblem(w, http.StatusNotFound, "no development user "+body.Name)
		return
	}
	du := a.cfg.Dev.Users[i]
	u := login{sessionUser: sessionUser{Name: du.Name, Email: du.Email, Groups: du.Groups, AuthTime: a.now().Truncate(time.Second)}, Method: config.AuthDev, ObjectID: du.ObjectID}
	if err := a.recordLogin(r.Context(), u); err != nil {
		a.refuseOrFail(w, r, err)
		return
	}
	if err := a.signIn(r.Context(), u.sessionUser); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Authenticator) fail(w http.ResponseWriter, r *http.Request, err error) {
	a.log.ErrorContext(r.Context(), "authentication", slog.Any("error", err))
	writeProblem(w, http.StatusInternalServerError, "")
}

// refuseOrFail answers a login recordLogin did not record: 403 with the reason when it refused
// the person, 500 otherwise.
func (a *Authenticator) refuseOrFail(w http.ResponseWriter, r *http.Request, err error) {
	var no refusal
	if !errors.As(err, &no) {
		a.fail(w, r, err)
		return
	}
	a.log.WarnContext(r.Context(), "login refused", slog.Any("error", err))
	writeProblem(w, http.StatusForbidden, no.msg)
}

// TokenPrefix starts every API token, so that leaked tokens are easy to find.
const TokenPrefix = "quai_"

// NewToken returns a new API token and the hash to store.
func NewToken() (token string, hash []byte) {
	token = TokenPrefix + random(32)
	return token, HashToken(token)
}

// HashToken returns the SHA-256 of a token, which is what the database keeps.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func (a *Authenticator) tokenIdentity(ctx context.Context, header string) (Identity, error) {
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || !strings.HasPrefix(tok, TokenPrefix) {
		return Identity{}, errors.New("the Authorization header must be Bearer and a Quai API token")
	}
	t, err := a.store.GetActiveAPIToken(ctx, HashToken(tok))
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, errors.New("the API token is unknown, expired or revoked")
	}
	if err != nil {
		return Identity{}, err
	}
	if err := a.store.TouchAPIToken(ctx, t.ID); err != nil {
		a.log.WarnContext(ctx, "api token: last use not recorded", slog.Any("error", err))
	}
	scopes := t.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return Identity{Principal: a.principal(t.Owner, t.Groups, scopes), TokenID: t.ID.String()}, nil
}

// random returns n random bytes, URL-safe base64 encoded.
func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(huma.ErrorModel{Title: http.StatusText(status), Status: status, Detail: detail})
}
