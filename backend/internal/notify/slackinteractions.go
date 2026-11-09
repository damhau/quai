package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/time/rate"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/secrets"
)

// SlackInteractions answers the requests Slack sends Quai when someone clicks a button of its
// messages (the app's interactivity, at auth.SlackInteractionsPath): Slack sends one even for a
// button that opens a link, and expects it acknowledged.
//
// It is the one endpoint a third party calls without a session or a token, so it verifies each
// request before anything else, with the app's signing secret as Slack documents: the body read
// within maxSlackInteraction, the signature v0=HMAC-SHA256(secret, "v0:" + timestamp + ":" +
// body) compared in constant time, the timestamp within five minutes of the server's clock either
// way. Every refusal gets the same answer (401, no body) and a log line without the body or the
// signature. A verified request is acknowledged (200) and nothing else: no database, no identity.
// The buttons open signed links, and what they open is decided in Quai, in the person's session
// (docs/design/05-delivery-and-approvals.md#approving-from-chat); a replayed request within the
// five minutes only gets another acknowledgement. Requests are rate limited (slackRate) so that a
// flood bounds its work: resolving the secret, hashing at most maxSlackInteraction.
type SlackInteractions struct {
	ref     string
	secrets *secrets.Store
	log     *slog.Logger
	now     func() time.Time
	limit   *rate.Limiter
}

// maxSlackInteraction bounds the body of an interaction request: Slack's payload repeats the
// message clicked, whose blocks Quai keeps under maxSlackBlocks, URL-encoded.
const maxSlackInteraction = 128 << 10

// slackSkew is how far from the server's clock a request's timestamp may be, either way.
const slackSkew = 5 * time.Minute

// slackRate and slackBurst bound the requests taken: 20 a second, 40 at once; more are refused
// with 429 before any work.
const (
	slackRate  = 20
	slackBurst = 40
)

// NewSlackInteractions returns the endpoint of a Slack app with a signing secret; nil without
// one: the endpoint is then absent.
func NewSlackInteractions(cfg *config.Slack, sec *secrets.Store, log *slog.Logger) *SlackInteractions {
	if cfg == nil || cfg.SigningSecret.Ref == "" {
		return nil
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &SlackInteractions{ref: cfg.SigningSecret.Ref, secrets: sec, log: log, now: time.Now, limit: rate.NewLimiter(slackRate, slackBurst)}
}

// UseClock sets the clock timestamps are checked by, for tests.
func (h *SlackInteractions) UseClock(now func() time.Time) { h.now = now }

func (h *SlackInteractions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.limit.Allow() {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	ts, sig, why := slackHeaders(r.Header)
	if why != "" {
		h.refuse(w, r, why)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSlackInteraction))
	if err != nil {
		h.refuse(w, r, "a body over 128 KiB, or one that could not be read")
		return
	}
	secret, err := h.secrets.Resolve(r.Context(), h.ref)
	if err != nil {
		h.log.ErrorContext(r.Context(), "a Slack interaction request was not verified: the signing secret could not be resolved", slog.String("error", err.Error()))
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if why := verifySlack([]byte(secret.Reveal()), ts, sig, body, h.now()); why != "" {
		h.refuse(w, r, why)
		return
	}
	// Acknowledged, and nothing else: the payload is not even read.
	w.WriteHeader(http.StatusOK)
}

// refuse answers a request that is not Slack's, the same way whatever the reason, which is logged
// without the body or the signature.
func (h *SlackInteractions) refuse(w http.ResponseWriter, r *http.Request, why string) {
	h.log.WarnContext(r.Context(), "a Slack interaction request was refused", slog.String("reason", why), slog.String("ip", auth.RemoteIP(r)))
	w.WriteHeader(http.StatusUnauthorized)
}

var (
	slackTimestampRe = regexp.MustCompile(`^[0-9]{1,12}$`)
	slackSignatureRe = regexp.MustCompile(`^v0=[0-9a-f]{64}$`)
)

// slackHeaders returns a request's timestamp and signature, each sent once and well formed, or
// why not.
func slackHeaders(h http.Header) (ts, sig, why string) {
	tss, sigs := h.Values("X-Slack-Request-Timestamp"), h.Values("X-Slack-Signature")
	switch {
	case len(tss) != 1 || len(sigs) != 1:
		return "", "", "no signature or timestamp, or more than one"
	case !slackTimestampRe.MatchString(tss[0]):
		return "", "", "a malformed timestamp"
	case !slackSignatureRe.MatchString(sigs[0]):
		return "", "", "a malformed signature"
	}
	return tss[0], sigs[0], ""
}

// verifySlack checks a request's signature and timestamp, and says why it is refused, or "".
func verifySlack(secret []byte, ts, sig string, body []byte, now time.Time) string {
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "a malformed timestamp"
	}
	if d := now.Sub(time.Unix(sec, 0)); d > slackSkew || d < -slackSkew {
		return "a timestamp more than five minutes from the server's clock"
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return "a signature that does not match"
	}
	return ""
}
