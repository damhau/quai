package chatlink

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	key1     = bytes.Repeat([]byte("k"), MinKeyLength)
	key2     = bytes.Repeat([]byte("l"), MinKeyLength)
	testHash = "sha256:" + strings.Repeat("ab", 32)
)

func signer(current []byte, previous ...[]byte) *Signer {
	return New(func(context.Context) ([]byte, [][]byte, error) { return current, previous, nil })
}

func link() Link {
	return Link{Request: uuid.MustParse("6f1c7a50-0d8e-4c55-9d0b-2b3a8c1e9f10"), Approver: "alice", Decision: Approve, ChangeSet: testHash, Channel: "teams"}
}

func code(t *testing.T, err error) string {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("not a refusal: %v", err)
	}
	return r.Code
}

// TestLink: a link opens with the key that made it, or a previous one, until it expires; it binds
// every field.
func TestLink(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC)
	s := signer(key1)
	s.UseClock(func() time.Time { return now })
	tok, err := s.Make(ctx, link())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "v1.") || len(tok) > MaxLength || strings.ContainsAny(tok, "+/=") {
		t.Errorf("token %q", tok)
	}
	got, err := s.Open(ctx, tok)
	want := link()
	want.Expires = now.Add(Lifetime)
	if err != nil || got.Request != want.Request || got.Approver != want.Approver || got.Decision != want.Decision ||
		got.ChangeSet != want.ChangeSet || got.Channel != want.Channel || !got.Expires.Equal(want.Expires) {
		t.Fatalf("Open = %+v, %v; want %+v", got, err, want)
	}

	// After a rotation, the previous key still opens it; without it, nothing does.
	rotated := signer(key2, key1)
	rotated.UseClock(s.now)
	if _, err := rotated.Open(ctx, tok); err != nil {
		t.Errorf("with the previous key: %v", err)
	}
	if _, err := signer(key2).Open(ctx, tok); code(t, err) != Signature {
		t.Errorf("without the key: %v", err)
	}

	// It expires.
	now = now.Add(Lifetime)
	if _, err := s.Open(ctx, tok); code(t, err) != Expired {
		t.Errorf("expired: %v", err)
	}
}

// TestLinkTampered: changing any field, or the MAC, or the format, is refused, as is a link
// whose expiry was pushed back.
func TestLinkTampered(t *testing.T) {
	ctx := context.Background()
	s := signer(key1)
	tok, err := s.Make(ctx, link())
	if err != nil {
		t.Fatal(err)
	}
	payload, sum, _ := strings.Cut(strings.TrimPrefix(tok, "v1."), ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	for field, v := range map[string]any{"r": uuid.NewString(), "a": "mallory", "d": Deny, "h": "sha256:" + strings.Repeat("cd", 32), "c": "slack",
		"e": p["e"].(float64) + 3600} {
		q := map[string]any{}
		for k, x := range p {
			q[k] = x
		}
		q[field] = v
		b, _ := json.Marshal(q)
		forged := "v1." + base64.RawURLEncoding.EncodeToString(b) + "." + sum
		if _, err := s.Open(ctx, forged); code(t, err) != Signature {
			t.Errorf("%s changed: %v", field, err)
		}
	}
	for name, bad := range map[string]string{
		"empty":         "",
		"no version":    strings.TrimPrefix(tok, "v1."),
		"another":       "v2." + strings.TrimPrefix(tok, "v1."),
		"no mac":        "v1." + payload,
		"short mac":     "v1." + payload + "." + sum[:10],
		"padded":        tok + "=",
		"not base64":    "v1.!!!." + sum,
		"too long":      tok + strings.Repeat("A", MaxLength),
		"unknown field": "v1." + base64.RawURLEncoding.EncodeToString([]byte(`{"r":"`+link().Request.String()+`","a":"alice","d":"approve","h":"`+testHash+`","c":"teams","e":1,"x":1}`)) + "." + sum,
		"bad decision":  "v1." + base64.RawURLEncoding.EncodeToString([]byte(`{"r":"`+link().Request.String()+`","a":"alice","d":"merge","h":"`+testHash+`","c":"teams","e":1}`)) + "." + sum,
	} {
		if _, err := s.Open(ctx, bad); code(t, err) != Malformed {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestLinkCheck: a link is refused for another request, person or decision, a request no longer
// waiting, or another change set.
func TestLinkCheck(t *testing.T) {
	l := link()
	ok := Use{Request: l.Request, Approver: "alice", Decision: Approve, Pending: true, ChangeSet: testHash}
	if r := l.Check(ok); r != nil {
		t.Fatalf("Check = %v", r)
	}
	opening := ok
	opening.Decision = ""
	if r := l.Check(opening); r != nil {
		t.Errorf("opening: %v", r)
	}
	for want, u := range map[string]func(*Use){
		Request:  func(u *Use) { u.Request = uuid.New() },
		Approver: func(u *Use) { u.Approver = "bob" },
		Decision: func(u *Use) { u.Decision = Deny },
		Decided:  func(u *Use) { u.Pending = false },
		Changed:  func(u *Use) { u.ChangeSet = "sha256:" + strings.Repeat("00", 32) },
	} {
		use := ok
		u(&use)
		if r := l.Check(use); r == nil || r.Code != want {
			t.Errorf("%s: %v", want, r)
		}
	}
}

// TestLinkOnBehalf: a link for a delegate's decision on a delegator's behalf binds the delegator
// too: removing, adding or changing them breaks the MAC, and a decision on behalf of anyone else,
// or in the delegate's own right, is refused. A link without one keeps the MAC links had before.
func TestLinkOnBehalf(t *testing.T) {
	ctx := context.Background()
	s := signer(key1)
	l := link()
	l.Approver, l.OnBehalfOf = "fiona", "alice"
	tok, err := s.Make(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Open(ctx, tok)
	if err != nil || got.Approver != "fiona" || got.OnBehalfOf != "alice" {
		t.Fatalf("Open = %+v, %v", got, err)
	}
	payload, sum, _ := strings.Cut(strings.TrimPrefix(tok, "v1."), ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil || p["o"] != "alice" {
		t.Fatalf("payload %s: %v", raw, err)
	}
	for name, change := range map[string]func(map[string]any){
		"removed": func(q map[string]any) { delete(q, "o") },
		"changed": func(q map[string]any) { q["o"] = "carol" },
	} {
		q := maps.Clone(p)
		change(q)
		b, _ := json.Marshal(q)
		if _, err := s.Open(ctx, "v1."+base64.RawURLEncoding.EncodeToString(b)+"."+sum); code(t, err) != Signature {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Someone added to a link made for the approver's own decision.
	own, err := s.Make(ctx, link())
	if err != nil {
		t.Fatal(err)
	}
	payload, sum, _ = strings.Cut(strings.TrimPrefix(own, "v1."), ".")
	raw, _ = base64.RawURLEncoding.DecodeString(payload)
	p = map[string]any{}
	_ = json.Unmarshal(raw, &p)
	p["o"] = "carol"
	b, _ := json.Marshal(p)
	if _, err := s.Open(ctx, "v1."+base64.RawURLEncoding.EncodeToString(b)+"."+sum); code(t, err) != Signature {
		t.Errorf("added: %v", err)
	}
	// The MAC of a link for one's own decision is the one links had before delegation.
	want := hmac.New(sha256.New, key1)
	for _, f := range []string{domain, got.Request.String(), "alice", Approve, testHash, "teams", strconv.FormatInt(got.Expires.Unix(), 10)} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(f))) // #nosec G115 -- short test fields
		want.Write(n[:])
		want.Write([]byte(f))
	}
	ownLink := link()
	ownLink.Expires = got.Expires
	if !hmac.Equal(mac(key1, ownLink), want.Sum(nil)) {
		t.Error("a link without a delegator has another MAC than before")
	}

	use := Use{Request: l.Request, Approver: "fiona", Decision: Approve, OnBehalfOf: "alice", Pending: true, ChangeSet: testHash}
	if r := got.Check(use); r != nil {
		t.Errorf("Check = %v", r)
	}
	opening := use
	opening.Decision, opening.OnBehalfOf = "", ""
	if r := got.Check(opening); r != nil {
		t.Errorf("opening: %v", r)
	}
	for _, who := range []string{"", "carol"} {
		u := use
		u.OnBehalfOf = who
		if r := got.Check(u); r == nil || r.Code != Behalf {
			t.Errorf("on behalf of %q: %v", who, r)
		}
	}
	// The delegator cannot use the delegate's link, nor anyone a link for their own decision on
	// someone's behalf.
	u := use
	u.Approver = "alice"
	if r := got.Check(u); r == nil || r.Code != Approver {
		t.Errorf("the delegator: %v", r)
	}
	if r := link().Check(Use{Request: l.Request, Approver: "alice", Decision: Approve, OnBehalfOf: "dora", Pending: true, ChangeSet: testHash}); r == nil || r.Code != Behalf {
		t.Errorf("an own link on behalf: %v", r)
	}
	self := l
	self.OnBehalfOf = "fiona"
	if _, err := s.Make(ctx, self); err == nil {
		t.Error("a link on one's own behalf")
	}
}

// TestLinkKeys: keys shorter than 256 bits, or none, make and open nothing; neither do invalid
// fields.
func TestLinkKeys(t *testing.T) {
	ctx := context.Background()
	if _, err := signer([]byte("short")).Make(ctx, link()); err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Errorf("short key: %v", err)
	}
	if _, err := signer(key1, []byte("short")).Make(ctx, link()); err == nil || !strings.Contains(err.Error(), "previousKeys[0]") {
		t.Errorf("short previous key: %v", err)
	}
	var none *Signer
	if _, err := none.Make(ctx, link()); err == nil {
		t.Error("no signer made a link")
	}
	l := link()
	l.ChangeSet = "abc"
	if _, err := signer(key1).Make(ctx, l); err == nil {
		t.Error("a link with a bad hash was made")
	}
}
