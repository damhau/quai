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

	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/store"
)

// Identity is who makes a request.
type Identity struct {
	Principal authz.Principal
	Email     string
	// CSRFToken is the session's token, for the client to send back; empty with an API token.
	CSRFToken string
	// TokenID is the API token used, if any.
	TokenID string
	// AuthTime is when the person last authenticated with the IdP, for step-up checks.
	AuthTime time.Time
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

// sessionUser is what a session remembers about its user.
type sessionUser struct {
	Name     string    `json:"name"`
	Email    string    `json:"email,omitempty"`
	Groups   []string  `json:"groups"`
	AuthTime time.Time `json:"authTime"`
}

// Session keys.
const (
	keyUser     = "user"
	keyCSRF     = "csrf"
	keyState    = "oidc_state"
	keyNonce    = "oidc_nonce"
	keyVerifier = "oidc_verifier"
	keyNext     = "oidc_next"
)

// Authenticator signs people in and identifies requests.
type Authenticator struct {
	cfg      config.Auth
	sessions *scs.SessionManager
	store    *store.Store
	log      *slog.Logger

	mu       sync.Mutex
	provider *oidc.Provider
}

// New returns an authenticator keeping sessions in s's database.
func New(cfg *config.Config, s *store.Store, log *slog.Logger) *Authenticator {
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
	return &Authenticator{cfg: cfg.Auth, sessions: sm, store: s, log: log}
}

// Middleware loads the session and identifies the request: an API token in the Authorization
// header, or else the session's user. It refuses a state-changing request made with a session
// but without its CSRF token.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	identify := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if h := r.Header.Get("Authorization"); h != "" {
			id, err := a.tokenIdentity(ctx, h)
			if err != nil {
				writeProblem(w, http.StatusUnauthorized, err.Error())
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(ctx, id)))
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
			ctx = WithIdentity(ctx, id)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
	return a.sessions.LoadAndSave(identify)
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
	return Identity{
		Principal: a.principal(u.Name, u.Groups, nil),
		Email:     u.Email,
		CSRFToken: a.sessions.GetString(ctx, keyCSRF),
		AuthTime:  u.AuthTime,
	}, true
}

// principal gives a person the grants their groups have.
func (a *Authenticator) principal(name string, groups, scopes []string) authz.Principal {
	p := authz.Principal{Name: name, Groups: groups, Scopes: scopes}
	for _, m := range a.cfg.Roles {
		if !slices.Contains(groups, m.Group) {
			continue
		}
		scope := authz.Global
		if kind, n, ok := strings.Cut(m.Scope, ":"); ok {
			scope = authz.Scope{Kind: kind, Name: n}
		}
		g := authz.Grant{Role: authz.Role(m.Role), Scope: scope}
		if !slices.Contains(p.Grants, g) {
			p.Grants = append(p.Grants, g)
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
	if err := a.sessions.Destroy(r.Context()); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Authenticator) oauth(ctx context.Context) (*oidc.Provider, *oauth2.Config, error) {
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
		Scopes:       a.cfg.OIDC.Scopes,
	}, nil
}

// login sends the browser to the identity provider, remembering what the callback must check.
func (a *Authenticator) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, oc, err := a.oauth(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	state, nonce, verifier := random(24), random(24), oauth2.GenerateVerifier()
	a.sessions.Put(ctx, keyState, state)
	a.sessions.Put(ctx, keyNonce, nonce)
	a.sessions.Put(ctx, keyVerifier, verifier)
	a.sessions.Put(ctx, keyNext, localPath(r.URL.Query().Get("next")))
	http.Redirect(w, r, oc.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// callback finishes the login: it checks the state, exchanges the code with the PKCE verifier,
// verifies the ID token and its nonce, and signs the user in.
func (a *Authenticator) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	state := a.sessions.PopString(ctx, keyState)
	nonce := a.sessions.PopString(ctx, keyNonce)
	verifier := a.sessions.PopString(ctx, keyVerifier)
	next := a.sessions.PopString(ctx, keyNext)
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
	tok, err := oc.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		a.log.WarnContext(ctx, "login: code exchange failed", slog.Any("error", err))
		writeProblem(w, http.StatusUnauthorized, "the identity provider did not accept the login")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := provider.Verifier(&oidc.Config{ClientID: a.cfg.OIDC.ClientID}).Verify(ctx, raw)
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
		writeProblem(w, http.StatusForbidden, err.Error())
		return
	}
	if err := a.signIn(ctx, u); err != nil {
		a.fail(w, r, err)
		return
	}
	a.log.InfoContext(ctx, "login", slog.String("user", u.Name), slog.Int("groups", len(u.Groups)))
	http.Redirect(w, r, next, http.StatusFound)
}

func (a *Authenticator) userFromClaims(idt *oidc.IDToken) (sessionUser, error) {
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return sessionUser{}, err
	}
	if names, ok := claims["_claim_names"].(map[string]any); ok && names[a.cfg.OIDC.GroupsClaim] != nil {
		return sessionUser{}, errors.New("your account is in more groups than the ID token can list; " +
			"ask an administrator to emit only the groups assigned to Quai, or to use app roles")
	}
	u := sessionUser{AuthTime: time.Now()}
	for _, c := range []string{a.cfg.OIDC.NameClaim, "preferred_username", "email", "sub"} {
		if s, _ := claims[c].(string); s != "" {
			u.Name = s
			break
		}
	}
	u.Email, _ = claims["email"].(string)
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
	if at, ok := claims["auth_time"].(float64); ok {
		u.AuthTime = time.Unix(int64(at), 0)
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
	u := a.cfg.Dev.Users[i]
	if err := a.signIn(r.Context(), sessionUser{Name: u.Name, Groups: u.Groups, AuthTime: time.Now()}); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Authenticator) fail(w http.ResponseWriter, r *http.Request, err error) {
	a.log.ErrorContext(r.Context(), "authentication", slog.Any("error", err))
	writeProblem(w, http.StatusInternalServerError, "")
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

// localPath keeps a redirect target only when it is a path on this server.
func localPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") || strings.ContainsAny(p, "\r\n") {
		return "/"
	}
	return p
}

func writeProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(huma.ErrorModel{Title: http.StatusText(status), Status: status, Detail: detail})
}
