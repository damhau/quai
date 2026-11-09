// Package chatlink makes and opens the links of the approval cards Quai sends in chat (Microsoft
// Teams, Slack): a link opens a request with a decision ready, for one approver and one change
// set, until it expires (docs/design/05-delivery-and-approvals.md#approving-from-chat).
//
// A link is not a credential (open question 22). Opening it changes nothing: the request page
// shows the decision ready, and the decision is made by the review API, in a browser session as
// the approver, with every check of a review (CanReview, step-up, the change set) and the link's
// on top: its MAC and expiry (Open), then the request, the approver signed in, the decision, on
// whose behalf, the request still waiting and its change set unchanged (Check). A link sent to a
// delegate for a delegator's approval stays the delegate's: it names both, and the review it
// confirms is the delegate's, on the delegator's behalf
// (docs/design/05-delivery-and-approvals.md#delegation).
//
// A link is "v1.", its fields in JSON, base64url, ".", and their MAC, base64url. The MAC is
// HMAC-SHA256 over a canonical encoding of every field (a domain tag, then each field with its
// length; on whose behalf only when the link names someone, so that links made before delegation
// still open) under notifications.links.key, a secret of its own: never the session's or the CSRF
// key. Keys rotate by naming the new one key and the old one among previousKeys for a Lifetime;
// a link made with a key Quai no longer has is refused (open question 61).
package chatlink

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Lifetime is how long a link opens its decision after the card was sent: a card can wait in
// chat overnight, and the link never outlives its decision anyway, since it is refused once the
// request is decided or its content changes (open question 61).
const Lifetime = 24 * time.Hour

// MinKeyLength is the shortest key Quai signs links with: 256 bits.
const MinKeyLength = 32

// MaxLength bounds a link's token: anything longer is refused before it is decoded.
const MaxLength = 1024

// The decisions a link may carry.
const (
	Approve = "approve"
	Deny    = "deny"
)

// Channels are the chat channels links are made for.
var Channels = []string{"teams", "slack"}

// prefix is the token's version.
const prefix = "v1."

// domain separates these MACs from any other use of the key.
const domain = "quai/chat-link/v1"

// Link is what a link is bound to.
type Link struct {
	Request   uuid.UUID
	Approver  string // the person's name in the directory
	Decision  string // Approve or Deny
	ChangeSet string // the hash of the change set the card showed
	Channel   string // teams, slack
	Expires   time.Time
	// OnBehalfOf is the delegator whose approval the approver gives through a delegation; empty
	// for a review in the approver's own right.
	OnBehalfOf string
}

// Refusal is a link refused, and why: Code for the audit log, Reason for the person.
type Refusal struct {
	Code, Reason string
}

func (r *Refusal) Error() string { return "the link is refused: " + r.Reason }

// The codes of refusals.
const (
	Malformed = "malformed" // not a link Quai made
	Signature = "signature" // changed, or made with a key Quai no longer has
	Expired   = "expired"
	Request   = "request"  // for another request
	Approver  = "approver" // for another person
	Decision  = "decision" // a decision other than the link's
	Behalf    = "behalf"   // on someone's behalf other than the link's
	Decided   = "decided"  // the request no longer waits for approval
	Changed   = "changed"  // the request's change set changed since the card was sent
	Session   = "session"  // not from a browser session (an API token)
)

func refused(code, reason string) *Refusal { return &Refusal{Code: code, Reason: reason} }

// Keys returns the key links are made with, and the keys before it, which still open the links
// they made. Each is at least MinKeyLength bytes.
type Keys func(ctx context.Context) (current []byte, previous [][]byte, err error)

// Signer makes and opens links.
type Signer struct {
	keys Keys
	now  func() time.Time
}

// New returns a signer with keys, resolved at each use, so a rotated secret is used without a
// restart.
func New(keys Keys) *Signer { return &Signer{keys: keys, now: time.Now} }

// UseClock sets the clock links expire by, for tests.
func (s *Signer) UseClock(now func() time.Time) { s.now = now }

// payload is a link's fields as the token carries them.
type payload struct {
	Request   string `json:"r"`
	Approver  string `json:"a"`
	Decision  string `json:"d"`
	ChangeSet string `json:"h"`
	Channel   string `json:"c"`
	Expires   int64  `json:"e"`
	Behalf    string `json:"o,omitempty"`
}

// Make returns the token of a link expiring a Lifetime from now.
func (s *Signer) Make(ctx context.Context, l Link) (string, error) {
	key, _, err := s.resolve(ctx)
	if err != nil {
		return "", err
	}
	l.Expires = s.now().Add(Lifetime).Truncate(time.Second)
	if err := valid(l); err != nil {
		return "", err
	}
	p, err := json.Marshal(payload{Request: l.Request.String(), Approver: l.Approver, Decision: l.Decision, ChangeSet: l.ChangeSet,
		Channel: l.Channel, Expires: l.Expires.Unix(), Behalf: l.OnBehalfOf})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return prefix + enc.EncodeToString(p) + "." + enc.EncodeToString(mac(key, l)), nil
}

// Open returns the link a token carries when Quai made it and it has not expired; otherwise a
// *Refusal (Malformed, Signature, Expired), or the error of resolving the keys. The expiry is
// read only once the MAC matched.
func (s *Signer) Open(ctx context.Context, token string) (Link, error) {
	l, sum, ok := parse(token)
	if !ok {
		return Link{}, refused(Malformed, "this is not a link Quai made")
	}
	key, previous, err := s.resolve(ctx)
	if err != nil {
		return Link{}, err
	}
	matched := false
	for _, k := range append([][]byte{key}, previous...) {
		// Every key is tried: how many there are is all the time tells.
		if hmac.Equal(mac(k, l), sum) {
			matched = true
		}
	}
	if !matched {
		return Link{}, refused(Signature, "this link's signature does not match: it was changed, or made with a key Quai no longer has")
	}
	if !s.now().Before(l.Expires) {
		return Link{}, refused(Expired, "this link expired "+l.Expires.UTC().Format("2006-01-02 15:04 MST")+": review the request here")
	}
	return l, nil
}

// Use is what a link is used for: the request, the person signed in, the decision they make and on
// whose behalf, and the request as it is now.
type Use struct {
	Request    uuid.UUID
	Approver   string
	Decision   string // empty when only opening the link
	OnBehalfOf string // whose approval the decision gives ("": the approver's own), when deciding
	Pending    bool   // the request waits for approval
	ChangeSet  string // its current change set's hash
}

// Check returns why a link opened with Open cannot be used so, or nil: it is for another
// request, another person, another decision or a decision on someone else's behalf, the request no
// longer waits for approval, or its change set is no longer the one the card showed.
func (l Link) Check(u Use) *Refusal {
	switch {
	case l.Request != u.Request:
		return refused(Request, "this link is for another request")
	case l.Approver != u.Approver:
		return refused(Approver, "this link was sent to someone else: review the request here as yourself")
	case u.Decision != "" && u.Decision != l.Decision:
		return refused(Decision, "the decision is not the link's: review the request here")
	case u.Decision != "" && u.OnBehalfOf != l.OnBehalfOf:
		return refused(Behalf, "the link is for a decision on someone else's behalf: review the request here")
	case !u.Pending:
		return refused(Decided, "the request no longer waits for approval")
	case l.ChangeSet != u.ChangeSet:
		return refused(Changed, "the request changed since the card was sent: review its new content here")
	}
	return nil
}

// resolve returns the keys, checked.
func (s *Signer) resolve(ctx context.Context) ([]byte, [][]byte, error) {
	if s == nil || s.keys == nil {
		return nil, nil, errors.New("chat links: no key (notifications.links.key)")
	}
	key, previous, err := s.keys(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("chat links' key: %w", err)
	}
	for i, k := range append([][]byte{key}, previous...) {
		if len(k) < MinKeyLength {
			name := "notifications.links.key"
			if i > 0 {
				name = fmt.Sprintf("notifications.links.previousKeys[%d]", i-1)
			}
			return nil, nil, fmt.Errorf("chat links: %s is %d bytes; it must be at least %d", name, len(k), MinKeyLength)
		}
	}
	return key, previous, nil
}

// mac is the MAC of a link: HMAC-SHA256 over the domain and each field, each with its length,
// the expiry in decimal Unix seconds, then on whose behalf when the link names someone: a link
// without has the MAC it had before delegation, and the field count tells them apart.
func mac(key []byte, l Link) []byte {
	h := hmac.New(sha256.New, key)
	fields := []string{domain, l.Request.String(), l.Approver, l.Decision, l.ChangeSet, l.Channel, strconv.FormatInt(l.Expires.Unix(), 10)}
	if l.OnBehalfOf != "" {
		fields = append(fields, l.OnBehalfOf)
	}
	for _, f := range fields {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(f))) // #nosec G115 -- fields are bounded by MaxLength
		h.Write(n[:])
		h.Write([]byte(f))
	}
	return h.Sum(nil)
}

// parse reads a token: its link and its MAC, or false when it is not one Quai makes.
func parse(token string) (Link, []byte, bool) {
	rest, ok := strings.CutPrefix(token, prefix)
	if !ok || len(token) > MaxLength {
		return Link{}, nil, false
	}
	p64, s64, ok := strings.Cut(rest, ".")
	if !ok {
		return Link{}, nil, false
	}
	enc := base64.RawURLEncoding.Strict()
	raw, err := enc.DecodeString(p64)
	if err != nil {
		return Link{}, nil, false
	}
	sum, err := enc.DecodeString(s64)
	if err != nil || len(sum) != sha256.Size {
		return Link{}, nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p payload
	if err := dec.Decode(&p); err != nil || dec.More() {
		return Link{}, nil, false
	}
	id, err := uuid.Parse(p.Request)
	if err != nil || id.String() != p.Request {
		return Link{}, nil, false
	}
	l := Link{Request: id, Approver: p.Approver, Decision: p.Decision, ChangeSet: p.ChangeSet, Channel: p.Channel, Expires: time.Unix(p.Expires, 0),
		OnBehalfOf: p.Behalf}
	if valid(l) != nil {
		return Link{}, nil, false
	}
	return l, sum, true
}

// valid checks a link's fields.
func valid(l Link) error {
	switch {
	case l.Request == uuid.Nil:
		return errors.New("chat link: no request")
	case l.Approver == "" || len(l.Approver) > 256:
		return errors.New("chat link: no approver")
	case len(l.OnBehalfOf) > 256 || l.OnBehalfOf == l.Approver:
		return errors.New("chat link: not on someone else's behalf")
	case l.Decision != Approve && l.Decision != Deny:
		return fmt.Errorf("chat link: %q is not a decision", l.Decision)
	case !hash(l.ChangeSet):
		return errors.New("chat link: the change set's hash is not a SHA-256")
	case !slices.Contains(Channels, l.Channel):
		return fmt.Errorf("chat link: %q is not a chat channel", l.Channel)
	case l.Expires.Unix() <= 0:
		return errors.New("chat link: no expiry")
	}
	return nil
}

// hash reports whether s is a change set's hash: sha256: and 64 lowercase hexadecimal digits.
func hash(s string) bool {
	h, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(h) != 2*sha256.Size || strings.ToLower(h) != h {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}
