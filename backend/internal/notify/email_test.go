package notify

import (
	"context"
	"errors"
	"net/smtp"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/mailtest"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store/db"
)

// emailTo returns the email channel to a Mailpit, trusting its CA.
func emailTo(t *testing.T, srv *mailtest.Server, transport string, edit ...func(*config.Email)) *Email {
	t.Helper()
	network, err := netx.New(srv.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Email{Host: srv.Host, Port: srv.Port, Transport: transport, From: "Quai <quai@example.com>"}
	for _, f := range edit {
		f(&cfg)
	}
	sec := secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: os.LookupEnv}})
	e, err := NewEmail(cfg, network, sec, "https://quai.example.com")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func message() Message {
	return Message{Kind: Submitted, Subject: "[Quai] Submitted: #7 Create bucket", Headline: "Your request #7 was submitted.",
		Request: &RequestContent{Number: 7, Title: "Create bucket", Repository: "infra-live", Requester: "alice", Reason: "for the shop"},
		Link:    "https://quai.example.com/requests/0b6e4c43-9a0f-4f53-9d1b-3c4f1f6c2a11"}
}

func send(t *testing.T, e *Email, to Recipient, m Message) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return e.Send(ctx, to, m)
}

func TestEmailSTARTTLS(t *testing.T) {
	t.Parallel()
	srv := mailtest.Start(t, mailtest.Options{Transport: mailtest.STARTTLS})
	e := emailTo(t, srv, config.EmailSTARTTLS)
	if err := send(t, e, Recipient{ID: "user:alice", Name: "Alice Martin", Address: "alice@example.com"}, message()); err != nil {
		t.Fatal(err)
	}
	list := srv.Await(t, 1, 10*time.Second)
	m := srv.Message(t, list[0].ID)
	if m.Subject != "[Quai] Submitted: #7 Create bucket" || m.From.Address != "quai@example.com" || m.From.Name != "Quai" ||
		len(m.To) != 1 || m.To[0].Address != "alice@example.com" || m.To[0].Name != "Alice Martin" {
		t.Fatalf("message = %+v", m.Summary)
	}
	for _, part := range []string{m.Text, m.HTML} {
		for _, want := range []string{"Your request #7 was submitted.", "for the shop", "infra-live",
			"https://quai.example.com/requests/0b6e4c43-9a0f-4f53-9d1b-3c4f1f6c2a11"} {
			if !strings.Contains(part, want) {
				t.Errorf("a part lacks %q:\n%s", want, part)
			}
		}
	}
	raw := srv.Raw(t, list[0].ID)
	for _, want := range []string{"Auto-Submitted: auto-generated", "Content-Type: multipart/alternative", "Message-ID: <"} {
		if !strings.Contains(raw, want) {
			t.Errorf("the message lacks %q", want)
		}
	}
}

func TestEmailImplicitTLS(t *testing.T) {
	t.Parallel()
	srv := mailtest.Start(t, mailtest.Options{Transport: mailtest.TLS})
	e := emailTo(t, srv, config.EmailTLS)
	if err := send(t, e, Recipient{ID: "user:alice", Address: "alice@example.com"}, message()); err != nil {
		t.Fatal(err)
	}
	srv.Await(t, 1, 10*time.Second)
}

// A server that does not offer STARTTLS, or someone on the way who removed it from its answer,
// gets nothing: no fall back to plaintext.
func TestEmailNoPlaintext(t *testing.T) {
	t.Parallel()
	srv := mailtest.Start(t, mailtest.Options{Transport: mailtest.Plain})
	e := emailTo(t, srv, config.EmailSTARTTLS)
	err := send(t, e, Recipient{ID: "user:alice", Address: "alice@example.com"}, message())
	if err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Fatalf("err = %v, want no STARTTLS", err)
	}
	if n := len(srv.Messages(t)); n != 0 {
		t.Fatalf("%d messages went in plaintext", n)
	}
}

// The server's certificate is verified: one from a CA Quai does not trust is refused, over
// STARTTLS and implicit TLS.
func TestEmailUntrustedCertificate(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{mailtest.STARTTLS, mailtest.TLS} {
		srv := mailtest.Start(t, mailtest.Options{Transport: transport})
		network, err := netx.New("") // the system's CAs only
		if err != nil {
			t.Fatal(err)
		}
		e, err := NewEmail(config.Email{Host: srv.Host, Port: srv.Port, Transport: transport, From: "quai@example.com"}, network, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		err = send(t, e, Recipient{ID: "user:alice", Address: "alice@example.com"}, message())
		if err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("%s: err = %v, want an untrusted certificate", transport, err)
		}
		if n := len(srv.Messages(t)); n != 0 {
			t.Fatalf("%s: %d messages sent", transport, n)
		}
	}
}

func TestEmailAuthentication(t *testing.T) {
	t.Parallel()
	const password = "s3cret-pass-word"
	srv := mailtest.Start(t, mailtest.Options{Transport: mailtest.STARTTLS, Username: "quai", Password: password})
	ref := func(v string) func(*config.Email) {
		return func(c *config.Email) {
			c.Username, c.Password = "quai", config.SecretRef{Ref: "env:QUAI_TEST_SMTP_" + strings.ToUpper(strings.ReplaceAll(v, "-", "_"))}
		}
	}
	// t.Setenv refuses parallel tests: the values go in a lookup of their own.
	env := map[string]string{"QUAI_TEST_SMTP_GOOD": password, "QUAI_TEST_SMTP_BAD": "wrong-" + password}
	lookup := secrets.Env{Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	for _, c := range []struct {
		name string
		ok   bool
	}{{"good", true}, {"bad", false}} {
		e := emailTo(t, srv, config.EmailSTARTTLS, ref(c.name))
		e.secrets = secrets.New(map[string]secrets.Backend{"env": lookup})
		err := send(t, e, Recipient{ID: "user:alice", Address: "alice@example.com"}, message())
		if c.ok && err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !c.ok {
			if err == nil {
				t.Fatalf("%s: sent with a wrong password", c.name)
			}
			if strings.Contains(err.Error(), password) {
				t.Fatalf("%s: the error holds the password: %v", c.name, err)
			}
		}
	}
	if n := len(srv.Await(t, 1, 10*time.Second)); n != 1 {
		t.Fatalf("%d messages, want 1", n)
	}
}

// Names, titles and reasons come from people and the identity provider: none can add a header
// or a recipient, nor script the HTML part.
func TestEmailHostileContent(t *testing.T) {
	t.Parallel()
	srv := mailtest.Start(t, mailtest.Options{Transport: mailtest.STARTTLS})
	e := emailTo(t, srv, config.EmailSTARTTLS)
	m := message()
	m.Subject = "[Quai] Submitted: #7 Create x\r\nBcc: victim@example.com\r\nX-Evil: yes"
	m.Request.Title = "x\r\nBcc: victim@example.com"
	m.Request.Reason = "<script>alert(1)</script><b>bold</b> & <a href=\"javascript:alert(2)\">click</a>"
	m.Request.Requester = "eve<img src=x onerror=alert(3)>"
	to := Recipient{ID: "user:eve", Name: "Zoë \"Eve\"\r\nBcc: victim@example.com", Address: "eve@example.com"}
	if err := send(t, e, to, m); err != nil {
		t.Fatal(err)
	}
	list := srv.Await(t, 1, 10*time.Second)
	if len(list) != 1 || len(list[0].To) != 1 || list[0].To[0].Address != "eve@example.com" {
		t.Fatalf("messages = %+v", list)
	}
	raw := srv.Raw(t, list[0].ID)
	headers, _, _ := strings.Cut(raw, "\r\n\r\n")
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") || strings.HasPrefix(strings.ToLower(line), "x-evil:") {
			t.Fatalf("a header was injected: %q", line)
		}
	}
	if !strings.Contains(headers, "=?utf-8?") {
		t.Errorf("the non-ASCII name is not encoded:\n%s", headers)
	}
	got := srv.Message(t, list[0].ID)
	if got.To[0].Name != "Zoë \"Eve\" Bcc: victim@example.com" {
		t.Errorf("name = %q", got.To[0].Name)
	}
	if got.Subject != "[Quai] Submitted: #7 Create x Bcc: victim@example.com X-Evil: yes" {
		t.Errorf("subject = %q", got.Subject)
	}
	for _, bad := range []string{"<script>", "<b>bold", "<img", "href=\"javascript", "<a href=\"javascript"} {
		if strings.Contains(got.HTML, bad) {
			t.Errorf("the HTML part holds %q:\n%s", bad, got.HTML)
		}
	}
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;b&gt;bold&lt;/b&gt;", "eve&lt;img"} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("the HTML part lacks %q:\n%s", want, got.HTML)
		}
	}
	if !strings.Contains(got.Text, "<script>alert(1)</script>") {
		t.Errorf("the text part changed the reason:\n%s", got.Text)
	}
}

func TestOneLine(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"a\r\nb":                          "a b",
		"a\nBcc: x":                       "a Bcc: x",
		"tab\there  and   spaces":         "tab here and spaces",
		"nul\x00x":                        "nul x",
		"ls" + string(rune(0x2028)) + "x": "ls x",
		strings.Repeat("é", 300):          strings.Repeat("é", 199) + "...",
	} {
		if got := oneLine(in, 200); got != want {
			t.Errorf("oneLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginAuth(t *testing.T) {
	t.Parallel()
	a := loginAuth{username: "quai", password: "pw"}
	if _, _, err := a.Start(&smtp.ServerInfo{Name: "mail", TLS: false}); err == nil {
		t.Fatal("LOGIN started without TLS")
	}
	if mech, _, err := a.Start(&smtp.ServerInfo{Name: "mail", TLS: true}); err != nil || mech != "LOGIN" {
		t.Fatalf("Start = %q, %v", mech, err)
	}
	for challenge, want := range map[string]string{"Username:": "quai", "Password:": "pw"} {
		got, err := a.Next([]byte(challenge), true)
		if err != nil || string(got) != want {
			t.Fatalf("Next(%q) = %q, %v", challenge, got, err)
		}
	}
	if _, err := a.Next([]byte("Other:"), true); err == nil {
		t.Fatal("an unknown challenge was answered")
	}
}

func TestRedact(t *testing.T) {
	t.Parallel()
	err := redact(errors.New("535 bad credentials for s3cret (czNjcmV0)"), "s3cret")
	if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "czNjcmV0") {
		t.Fatalf("err = %v", err)
	}
	if redact(nil, "x") != nil {
		t.Fatal("redact(nil) is not nil")
	}
}

func TestEmailAddress(t *testing.T) {
	t.Parallel()
	e := &Email{}
	for email, ok := range map[string]bool{"alice@example.com": true, "": false, "a@example.com\r\nBcc: b@example.com": false,
		"Alice <alice@example.com>": false, "a@example.com, b@example.com": false} {
		if _, got := e.Address(db.User{Name: "a", Email: email}); got != ok {
			t.Errorf("Address(%q) = %v, want %v", email, got, ok)
		}
	}
}
