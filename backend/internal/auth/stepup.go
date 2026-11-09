package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// ClockSkew is how far in the future an ID token's auth_time may be, the identity provider's
// clock being ahead of Quai's; beyond it, the authentication does not satisfy a step-up (open
// question 49).
const ClockSkew = time.Minute

// StepUpProblemType is the type of the problem an action refused for want of a step-up returns,
// for the UI to offer a new sign-in.
const StepUpProblemType = "https://quai.dev/problems/step-up-required"

// StepUpError is an action refused because the person's authentication is too old or too weak
// for it: they must sign in again (docs/design/08-identity-access-tenancy.md#step-up).
type StepUpError struct {
	Reason string
}

func (e *StepUpError) Error() string { return "this action needs a recent sign-in: " + e.Reason }

// ErrStepUp matches every *StepUpError with errors.Is.
var ErrStepUp = errors.New("step-up required")

// Is makes errors.Is(err, ErrStepUp) true.
func (e *StepUpError) Is(target error) bool { return target == ErrStepUp }

// StepUp decides whether a person authenticated recently enough, and strongly enough, for an
// action that needs a step-up: approving a request whose approval rules ask for it, breaking
// glass, and changing repository connections with auth.stepUp.admin. It is checked on the server
// when the action is made; the UI's prompt only saves a refusal.
type StepUp struct {
	cfg        config.StepUp
	configured bool
	store      *store.Store
	now        func() time.Time
}

// NewStepUp returns what decides step-ups with the settings of cfg, recording refusals in s's
// audit log (none when s is nil).
func NewStepUp(cfg config.Auth, s *store.Store) *StepUp {
	return &StepUp{cfg: cfg.StepUpSettings(), configured: cfg.StepUp != nil, store: s, now: time.Now}
}

// UseClock sets the clock step-ups are judged by, for tests.
func (s *StepUp) UseClock(now func() time.Time) { s.now = now }

// Configured reports whether auth.stepUp is set: break-glass needs a step-up whenever it is.
func (s *StepUp) Configured() bool { return s.configured }

// Admin reports whether changing repository connections needs a step-up (auth.stepUp.admin).
func (s *StepUp) Admin() bool { return s.configured && s.cfg.Admin }

// MaxAge is how long an authentication satisfies a step-up.
func (s *StepUp) MaxAge() time.Duration { return s.cfg.MaxAge }

// Check returns why who's authentication does not satisfy a step-up now, or "" when it does. Only
// what the identity provider said in the ID token counts: its auth_time, which must be there, at
// most ClockSkew in the future and at most MaxAge old; its acr, one of auth.stepUp.acrValues when
// set; its acrs, holding auth.stepUp.authContext when set. An API token never satisfies it.
func (s *StepUp) Check(who Identity) string {
	now := s.now()
	switch {
	case who.TokenID != "":
		return "an API token cannot step up; do this in Quai, signed in"
	case who.Principal.Name == "":
		return "nobody is signed in"
	case who.AuthTime.IsZero():
		return "the identity provider did not say when you authenticated (auth_time); sign in again"
	case who.AuthTime.After(now.Add(ClockSkew)):
		return "the identity provider's authentication time is in the future; sign in again, or ask an administrator to check the clocks"
	case now.Sub(who.AuthTime) > s.cfg.MaxAge:
		return fmt.Sprintf("you authenticated more than %s ago; sign in again", s.cfg.MaxAge)
	case len(s.cfg.ACRValues) > 0 && !slices.Contains(s.cfg.ACRValues, who.ACR):
		got := "none"
		if who.ACR != "" {
			got = strconv.Quote(who.ACR)
		}
		return fmt.Sprintf("your authentication's class (acr %s) is not one of %s; sign in again", got, strings.Join(s.cfg.ACRValues, ", "))
	case s.cfg.AuthContext != "" && !slices.Contains(who.ACRS, s.cfg.AuthContext):
		return fmt.Sprintf("your sign-in did not satisfy the authentication context %s (acrs); sign in again", s.cfg.AuthContext)
	}
	return ""
}

// Until returns when who's step-up stops satisfying, or the zero time when it does not now.
func (s *StepUp) Until(who Identity) time.Time {
	if s.Check(who) != "" {
		return time.Time{}
	}
	return who.AuthTime.Add(s.cfg.MaxAge)
}

// Require returns nil when who's authentication satisfies a step-up now. Otherwise it records the
// refusal in the audit log, auth.step_up_refused with what was attempted on obj and why, and
// returns a *StepUpError. Every action that needs a step-up calls it on the server, at the moment
// the action is made: reviews of step-up requests, repository changes with auth.stepUp.admin, and
// break-glass (P4-17) whenever Configured.
func (s *StepUp) Require(ctx context.Context, who Identity, attempted string, obj audit.Object) error {
	reason := s.Check(who)
	if reason == "" {
		return nil
	}
	if s.store != nil {
		ctx = audit.WithActor(ctx, ActorOf(ctx, who))
		if err := s.store.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			return audit.Add(ctx, tx, audit.Event{Action: "auth.step_up_refused", Object: obj,
				Details: map[string]any{"attempted": attempted, "reason": reason}})
		}); err != nil {
			return err
		}
	}
	return &StepUpError{Reason: reason}
}

// stepUpOptions are the authorization request's parameters of a step-up login: prompt=login and
// max_age, so that the person authenticates again and the ID token says when (auth_time is then
// required); acr_values, and Entra ID's authentication context in the claims parameter, when
// configured.
func (s *StepUp) stepUpOptions() []oauth2.AuthCodeOption {
	opts := []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam("prompt", "login"),
		oauth2.SetAuthURLParam("max_age", strconv.FormatInt(int64(s.cfg.MaxAge/time.Second), 10)),
	}
	if len(s.cfg.ACRValues) > 0 {
		opts = append(opts, oauth2.SetAuthURLParam("acr_values", strings.Join(s.cfg.ACRValues, " ")))
	}
	if s.cfg.AuthContext != "" {
		claims, _ := json.Marshal(map[string]any{"id_token": map[string]any{"acrs": map[string]any{"essential": true, "value": s.cfg.AuthContext}}})
		opts = append(opts, oauth2.SetAuthURLParam("claims", string(claims)))
	}
	return opts
}

// maxLocalPath is the longest redirect target kept.
const maxLocalPath = 2048

// localPath keeps a redirect target only when it is a path on this server, and returns / for
// anything else: it starts with a single /, holds no backslash and no control character, and names
// no scheme or host, however many times it is percent-decoded (/%2F/evil.example.com is
// //evil.example.com to some). A browser ignores tabs and newlines in a URL and reads \ as /, so
// /\t/host and /\host would leave the server.
func localPath(p string) string {
	if len(p) > maxLocalPath || !strings.HasPrefix(p, "/") {
		return "/"
	}
	s := p
	for range 4 {
		if strings.HasPrefix(s, "//") || strings.ContainsRune(s, '\\') || strings.ContainsFunc(s, unicode.IsControl) {
			return "/"
		}
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
			return "/"
		}
		d, err := url.PathUnescape(s)
		if err != nil {
			return "/"
		}
		if d == s {
			return p
		}
		s = d
	}
	// Still encoded after four decodings: nobody needs that.
	return "/"
}
