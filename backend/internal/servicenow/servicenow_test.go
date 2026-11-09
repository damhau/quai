package servicenow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/servicenowtest"
)

func clientOf(t *testing.T, sn *servicenowtest.Server, cfg *config.ServiceNow, ca string) *Client {
	t.Helper()
	network, err := netx.New(ca)
	if err != nil {
		t.Fatal(err)
	}
	sec := secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(name string) (string, bool) {
		switch name {
		case "SN_SECRET":
			return sn.ClientSecret, true
		case "SN_PASSWORD":
			return sn.Password, true
		}
		return "", false
	}}})
	c, err := New(*cfg, network, sec)
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func start(t *testing.T) (*servicenowtest.Server, *Client) {
	t.Helper()
	sn := servicenowtest.Start(t)
	return sn, clientOf(t, sn, sn.OAuth("env:SN_SECRET"), sn.CAFile)
}

func normal(correlation string) Change {
	return Change{Create: map[string]string{"short_description": "Quai #1: Create web01", "correlation_id": correlation},
		Update: map[string]string{"justification": "for the shop"}}
}

func TestCreateFindGet(t *testing.T) {
	sn, c := start(t)
	ctx := context.Background()
	if r, err := c.Find(ctx, "quai:1:1"); err != nil || r != nil {
		t.Fatalf("Find before = %v, %v", r, err)
	}
	r, err := c.Create(ctx, normal("quai:1:1"))
	if err != nil {
		t.Fatal(err)
	}
	if !SysIDRe.MatchString(r.SysID) || r.Number != "CHG0030001" || r.State != "-5" || r.Label() != "New" || r.Type != "normal" || r.Approval != "not requested" {
		t.Errorf("created %+v", r)
	}
	if got, _ := r.Outcome(); got != Pending {
		t.Errorf("a new change is %s", got)
	}
	found, err := c.Find(ctx, "quai:1:1")
	if err != nil || found == nil || found.SysID != r.SysID || found.Number != r.Number || found.State != "-5" {
		t.Fatalf("Find = %+v, %v", found, err)
	}
	if err := c.Update(ctx, r.SysID, map[string]string{"justification": "for the shop", "description": "the diff"}); err != nil {
		t.Fatal(err)
	}
	if err := c.WorkNote(ctx, r.SysID, "plan: [code]<img src=x>[/code]"); err != nil {
		t.Fatal(err)
	}
	moved, err := c.Move(ctx, r.SysID, "assess")
	if err != nil || moved.State != "-4" || moved.Approval != "requested" {
		t.Fatalf("Move = %+v, %v", moved, err)
	}
	sn.Approve(r.SysID)
	got, err := c.Get(ctx, r.SysID)
	if err != nil {
		t.Fatal(err)
	}
	if o, known := got.Outcome(); o != Approved || !known || got.Label() != "Scheduled" {
		t.Errorf("approved: %s %v %+v", o, known, got)
	}
	ch, _ := sn.Change(r.SysID)
	if ch.Fields["justification"] != "for the shop" || ch.Fields["description"] != "the diff" || ch.Fields["correlation_id"] != "quai:1:1" ||
		len(ch.WorkNotes) != 1 || ch.WorkNotes[0] != "plan: (code)&lt;img src=x&gt;(/code)" {
		t.Errorf("change = %+v", ch)
	}
	// One token for every call, cached; an expired one is renewed once.
	if n := sn.Tokens(); n != 1 {
		t.Errorf("%d tokens", n)
	}
	sn.ExpireTokens()
	if _, err := c.Get(ctx, r.SysID); err != nil {
		t.Fatal(err)
	}
	if n := sn.Tokens(); n != 2 {
		t.Errorf("%d tokens after expiry", n)
	}
	for _, call := range sn.Calls() {
		if call.Path != "/oauth_token.do" && call.Auth != "bearer" {
			t.Errorf("%s %s authenticated with %s", call.Method, call.Path, call.Auth)
		}
	}
	// A second change with the same correlation ID is ambiguous: Quai uses neither.
	other, err := c.Create(ctx, normal("quai:1:2"))
	if err != nil {
		t.Fatal(err)
	}
	sn.SetField(other.SysID, "correlation_id", "quai:1:1")
	if _, err := c.Find(ctx, "quai:1:1"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := c.Find(ctx, "quai^x=1"); err == nil {
		t.Error("a correlation ID with ^ was queried")
	}
}

func TestStandardChange(t *testing.T) {
	sn, c := start(t)
	ctx := context.Background()
	tmpl := sn.AddTemplate(map[string]string{"short_description": "Restart a VM", "implementation_plan": "the template's plan"})
	r, err := c.Create(ctx, Change{Template: tmpl, Create: map[string]string{"correlation_id": "quai:2:1", "implementation_plan": "Quai's", "no_such_field": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Type != "standard" || strings.Join(r.Ignored, ",") != "implementation_plan,no_such_field" {
		t.Errorf("standard = %+v", r)
	}
	ch, _ := sn.Change(r.SysID)
	if ch.Template != tmpl || ch.Fields["implementation_plan"] != "the template's plan" || ch.Fields["correlation_id"] != "quai:2:1" {
		t.Errorf("change = %+v", ch)
	}
	if _, err := c.Move(ctx, r.SysID, "scheduled"); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, r.SysID)
	if o, _ := got.Outcome(); err != nil || o != Approved {
		t.Errorf("scheduled: %s %+v %v", o, got, err)
	}
	// An unknown template is refused (404), not unreachable; a template that is not a sys_id is not
	// sent.
	_, err = c.Create(ctx, Change{Template: strings.Repeat("0", 32)})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound || Unreachable(err) {
		t.Errorf("unknown template: %v", err)
	}
	if _, err := c.Create(ctx, Change{Template: "../normal"}); err == nil {
		t.Error("a template that is not a sys_id was sent")
	}
}

func TestBasicAuthentication(t *testing.T) {
	sn := servicenowtest.Start(t)
	c := clientOf(t, sn, sn.Basic("env:SN_PASSWORD"), sn.CAFile)
	if _, err := c.Create(context.Background(), normal("quai:3:1")); err != nil {
		t.Fatal(err)
	}
	for _, call := range sn.Calls() {
		if call.Auth != "basic" {
			t.Errorf("%s %s authenticated with %s", call.Method, call.Path, call.Auth)
		}
	}
	// A wrong password is refused, and the error does not say it.
	sn.Password = "changed"
	_, err := c.Create(context.Background(), normal("quai:3:2"))
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized || Unreachable(err) {
		t.Errorf("wrong password: %v", err)
	}
}

func TestUnreachable(t *testing.T) {
	sn, c := start(t)
	ctx := context.Background()
	if _, err := c.Create(ctx, normal("quai:4:1")); err != nil {
		t.Fatal(err)
	}
	sn.Down(true)
	if _, err := c.Find(ctx, "quai:4:1"); !Unreachable(err) {
		t.Errorf("down: %v", err)
	}
	sn.Down(false)
	// Throttled within maxWait: retried; past it, unreachable.
	sn.Throttle(2, "1")
	if _, err := c.Find(ctx, "quai:4:1"); err != nil {
		t.Errorf("throttled twice: %v", err)
	}
	sn.Throttle(1, "3600")
	if _, err := c.Find(ctx, "quai:4:1"); !Unreachable(err) {
		t.Errorf("throttled an hour: %v", err)
	}
	sn.Throttle(3, "")
	if _, err := c.Find(ctx, "quai:4:1"); !Unreachable(err) {
		t.Errorf("throttled thrice: %v", err)
	}
	// A refused call is not unreachable.
	sn.Refuse("/api/now/table/", 1)
	if _, err := c.Find(ctx, "quai:4:1"); err == nil || Unreachable(err) || !strings.Contains(err.Error(), "HTTP 400: Invalid request") {
		t.Errorf("refused: %v", err)
	}
	// Nothing listens: unreachable, the error without the fields of the query.
	c.cfg.InstanceURL = "https://127.0.0.1:1"
	_, err := c.Create(ctx, Change{Create: map[string]string{"short_description": "secret-looking text"}})
	if !Unreachable(err) || strings.Contains(err.Error(), "secret-looking") {
		t.Errorf("no instance: %v", err)
	}
}

// TestCredentials: a wrong client secret is refused without the secret in the error; the token
// is never in one either.
func TestCredentials(t *testing.T) {
	sn := servicenowtest.Start(t)
	c := clientOf(t, sn, sn.OAuth("env:SN_PASSWORD"), sn.CAFile) // the password is not the client secret
	secret := sn.Password
	_, err := c.Find(context.Background(), "quai:5:1")
	if err == nil || strings.Contains(err.Error(), secret) || Unreachable(err) {
		t.Errorf("wrong secret: %v", err)
	}
	if err := redact(errors.New("Get x: token abc123 refused"), "abc123"); strings.Contains(err.Error(), "abc123") {
		t.Errorf("redact: %v", err)
	}
}

// TestNoRedirect: an answer redirecting elsewhere is not followed.
func TestNoRedirect(t *testing.T) {
	sn := servicenowtest.Start(t)
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sn.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	big := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result": "` + strings.Repeat("x", maxAnswer) + `"}`))
	}))
	t.Cleanup(big.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	pems := sn.PEM()
	for _, s := range []*httptest.Server{redirect, big} {
		pems = append(pems, (&servicenowtest.Server{Server: s}).PEM()...)
	}
	if err := os.WriteFile(ca, pems, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := sn.Basic("env:SN_PASSWORD")
	cfg.InstanceURL = redirect.URL
	c := clientOf(t, sn, cfg, ca)
	_, err := c.Find(context.Background(), "quai:6:1")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusFound {
		t.Errorf("redirect: %v", err)
	}
	if n := len(sn.Calls()); n != 0 {
		t.Errorf("the redirect was followed: %d calls", n)
	}
	cfg.InstanceURL = big.URL
	if _, err := clientOf(t, sn, cfg, ca).Find(context.Background(), "quai:6:1"); err == nil || !strings.Contains(err.Error(), "an answer over") {
		t.Errorf("a big answer: %v", err)
	}
}

func TestOutcome(t *testing.T) {
	for _, tt := range []struct {
		state, approval, want string
		known                 bool
	}{
		{"-5", "not requested", Pending, true},
		{"-4", "requested", Pending, true},
		{"-3", "requested", Pending, true},
		{"-3", "approved", Pending, true}, // approved, not scheduled yet
		{"-2", "approved", Approved, true},
		{"-1", "approved", Approved, true},
		{"0", "approved", Pending, true}, // in review: past its implementation
		{"-2", "requested", Pending, true},
		{"-5", "rejected", Rejected, true},
		{"-2", "rejected", Rejected, true},
		{"4", "approved", Cancelled, true},
		{"4", "requested", Cancelled, true},
		{"3", "approved", Closed, true},
		{"-2", "Approved", Pending, false}, // a display value, not the value
		{"-2", "approved_by_bot", Pending, false},
		{"7", "approved", Pending, false},
		{"", "approved", Pending, false},
		{"-2", "", Pending, false},
		{"scheduled", "approved", Pending, false},
	} {
		got, known := Record{State: tt.state, Approval: tt.approval}.Outcome()
		if got != tt.want || known != tt.known {
			t.Errorf("Outcome(%q, %q) = %s %v, want %s %v", tt.state, tt.approval, got, known, tt.want, tt.known)
		}
	}
}

func TestParse(t *testing.T) {
	sys := strings.Repeat("a", 32)
	r, err := Parse([]byte(`{"sys_id": {"value": "` + sys + `", "display_value": "` + sys + `"}, "state": {"value": 3.0, "display_value": "Closed"},
		"approval": {"value": "approved", "display_value": "Approved"}, "number": {"value": "CHG0000024"}, "correlation_id": {"value": "", "display_value": ""}}`))
	if err != nil || r.State != "3" || r.Label() != "Closed" || r.Number != "CHG0000024" || r.Approval != "approved" {
		t.Errorf("Parse = %+v, %v", r, err)
	}
	r, err = Parse([]byte(`{"sys_id": "` + sys + `", "state": "-2", "approval": "approved", "number": "<b>CHG1</b>"}`))
	if err != nil || r.State != "-2" || r.Number != "" || r.Label() != "Scheduled" {
		t.Errorf("Parse plain = %+v, %v", r, err)
	}
	for _, bad := range []string{`{}`, `{"sys_id": "abc"}`, `{"sys_id": {"value": 12}}`, `[]`, `null`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%s) took it", bad)
		}
	}
}

func FuzzParse(f *testing.F) {
	sys := strings.Repeat("0", 32)
	f.Add([]byte(`{"sys_id": {"value": "` + sys + `"}, "state": {"value": "-2"}, "approval": {"value": "approved"}}`))
	f.Add([]byte(`{"sys_id": "` + sys + `", "state": 3.0, "approval": "rejected", "__meta": {"ignoredFields": ["x"]}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := Parse(b)
		if err != nil {
			return
		}
		if !SysIDRe.MatchString(r.SysID) {
			t.Fatalf("a record with sys_id %q", r.SysID)
		}
		if o, _ := r.Outcome(); o == Approved && (r.Approval != "approved" || r.State != "-2" && r.State != "-1") {
			t.Fatalf("approved from %+v", r)
		}
	})
}

func TestText(t *testing.T) {
	for _, tt := range []struct {
		in   string
		max  int
		want string
	}{
		{"plain", 10, "plain"},
		{"[code]<script>alert(1)</script>[/code]", 100, "(code)&lt;script&gt;alert(1)&lt;/script&gt;(/code)"},
		{"[CODE]x[/ Code ]", 100, "(code)x(/code)"},
		{"[[code]code]x", 100, "[(code)code]x"},
		{"a & b", 100, "a &amp; b"},
		{"line\r\nnext\u0007\u202e\u2028end", 100, "line\nnextend"},
		{"abcdef", 4, "abc…"},
		{"a<b", 3, "a…"},
		{"ab<", 4, "ab…"},
		{"héllo", 5, "héllo"},
		{"\xffok", 5, "ok"},
	} {
		if got := Text(tt.in, tt.max); got != tt.want {
			t.Errorf("Text(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

func FuzzText(f *testing.F) {
	f.Add("[code]x[/code]", 50)
	f.Add("<a href=x>&amp;</a>", 10)
	f.Fuzz(func(t *testing.T, s string, max int) {
		if max < 1 || max > 5000 {
			return
		}
		got := Text(s, max)
		if codeTag.MatchString(got) || strings.ContainsAny(got, "<>") {
			t.Fatalf("Text(%q) = %q holds markup", s, got)
		}
		if n := len([]rune(got)); n > max {
			t.Fatalf("Text(%q, %d) has %d characters", s, max, n)
		}
	})
}
