package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Email sends notifications through an SMTP server (notifications.email), over TLS: STARTTLS,
// which the server must offer, or TLS from the first byte. The server's certificate is verified
// against the system's CAs and network.caFile; there is no way to turn that off, nor to send in
// plaintext (open question 52). The password is resolved from its secret reference at each
// send, and never logged nor in an error.
type Email struct {
	cfg     config.Email
	from    *mail.Address
	tls     *tls.Config
	secrets *secrets.Store
	helo    string // the EHLO name, and the domain of Message-IDs: server.publicURL's host
	now     func() time.Time
}

// NewEmail returns the email channel. Outgoing connections trust what network trusts.
func NewEmail(cfg config.Email, network *netx.Network, sec *secrets.Store, publicURL string) (*Email, error) {
	from, err := config.MailAddress(cfg.From, true)
	if err != nil {
		return nil, fmt.Errorf("notifications.email.from: %w", err)
	}
	if network == nil {
		if network, err = netx.New(""); err != nil {
			return nil, err
		}
	}
	helo := "localhost"
	if u, err := url.Parse(publicURL); err == nil && u.Hostname() != "" {
		helo = u.Hostname()
	}
	t := network.TLSConfig()
	t.ServerName = cfg.Host
	return &Email{cfg: cfg, from: from, tls: t, secrets: sec, helo: helo, now: time.Now}, nil
}

// Name names the channel.
func (e *Email) Name() string { return "email" }

// Address is a person's verified email (the directory keeps no other), when it is one address on
// one line.
func (e *Email) Address(u db.User) (string, bool) {
	if u.Email == "" {
		return "", false
	}
	if _, err := config.MailAddress(u.Email, false); err != nil {
		return "", false
	}
	return u.Email, true
}

// Send sends a message to one address.
func (e *Email) Send(ctx context.Context, to Recipient, m Message) error {
	if _, err := config.MailAddress(to.Address, false); err != nil {
		return err
	}
	body, err := e.message(to, m)
	if err != nil {
		return err
	}
	var password string
	if e.cfg.Username != "" {
		s, err := e.secrets.Resolve(ctx, e.cfg.Password.Ref)
		if err != nil {
			return fmt.Errorf("the SMTP password: %w", err) // names the reference, never the value
		}
		password = s.Reveal()
	}
	return redact(e.deliver(ctx, to.Address, body, password), password)
}

func (e *Email) deliver(ctx context.Context, to string, body []byte, password string) error {
	addr := net.JoinHostPort(e.cfg.Host, strconv.Itoa(e.cfg.Port))
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	var conn net.Conn
	var err error
	if e.cfg.Transport == config.EmailTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: e.tls}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to the SMTP server %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	c, err := smtp.NewClient(conn, e.cfg.Host)
	if err != nil {
		return fmt.Errorf("the SMTP server %s: %w", addr, err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Hello(e.helo); err != nil {
		return fmt.Errorf("the SMTP server %s: %w", addr, err)
	}
	if e.cfg.Transport == config.EmailSTARTTLS {
		// Never a fall back to plaintext: a server that does not offer STARTTLS, or someone on
		// the way who removed it, gets nothing.
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("the SMTP server %s does not offer STARTTLS: Quai sends no mail in plaintext", addr)
		}
		if err := c.StartTLS(e.tls); err != nil {
			return fmt.Errorf("STARTTLS with the SMTP server %s: %w", addr, err)
		}
	}
	if e.cfg.Username != "" {
		ok, mechanisms := c.Extension("AUTH")
		var a smtp.Auth
		switch {
		case !ok:
			return fmt.Errorf("the SMTP server %s offers no authentication, and notifications.email.username is set", addr)
		case hasWord(mechanisms, "PLAIN"):
			a = smtp.PlainAuth("", e.cfg.Username, password, e.cfg.Host)
		case hasWord(mechanisms, "LOGIN"):
			a = loginAuth{username: e.cfg.Username, password: password}
		default:
			return fmt.Errorf("the SMTP server %s offers neither PLAIN nor LOGIN authentication, but %s", addr, mechanisms)
		}
		if err := c.Auth(a); err != nil {
			return fmt.Errorf("authenticate to the SMTP server %s as %s: %w", addr, e.cfg.Username, err)
		}
	}
	if err := c.Mail(e.from.Address); err != nil {
		return fmt.Errorf("the SMTP server %s refused the sender: %w", addr, err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("the SMTP server %s refused the recipient: %w", addr, err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("the SMTP server %s: %w", addr, err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("the SMTP server %s: %w", addr, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the SMTP server %s refused the message: %w", addr, err)
	}
	// The message was accepted: a QUIT that fails does not undo it.
	_ = c.Quit()
	return nil
}

// hasWord reports whether a space-separated list holds a word, ignoring case.
func hasWord(list, word string) bool {
	for _, w := range strings.Fields(list) {
		if strings.EqualFold(w, word) {
			return true
		}
	}
	return false
}

// loginAuth is the LOGIN mechanism, which servers such as Microsoft 365's offer instead of PLAIN:
// only over TLS.
type loginAuth struct{ username, password string }

func (a loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("LOGIN authentication over an unencrypted connection")
	}
	return "LOGIN", nil, nil
}

func (a loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(challenge))) {
	case "username:", "user name", "username":
		return []byte(a.username), nil
	case "password:", "password":
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("an unexpected LOGIN challenge %q", challenge)
}

// redact removes a password, as it is and in base64, from an error's text.
func redact(err error, password string) error {
	if err == nil || password == "" {
		return err
	}
	msg := err.Error()
	clean := msg
	for _, p := range []string{password, base64.StdEncoding.EncodeToString([]byte(password))} {
		clean = strings.ReplaceAll(clean, p, "[redacted]")
	}
	if clean == msg {
		return err
	}
	return errors.New(clean)
}
