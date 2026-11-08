// Package audit is Quai's audit log (docs/design/09-security-and-compliance.md#audit-log): every
// security-relevant event, one after the other, each chained on the one before by its hash.
//
// An event is written in the transaction of what it records: code calls Add with the
// transaction store.Tx gives it, and store.Tx records the events just before it commits. Recording
// takes a transaction-level advisory lock, the last lock the transaction takes, so that seq has
// no gaps and each event chains on the last; the holder needs nothing more than its own
// connection to commit and release it.
//
// Personal data (people's names, IP addresses, what they typed) is in the event as a salted hash,
// its clear value in a separate table that pseudonymization deletes from without breaking the
// chain (open question 18).
package audit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/store/db"
)

// Actor types.
const (
	User   = "user"   // a person, signed in
	Token  = "token"  // a person's API token
	Runner = "runner" // a repository's runner, reporting with its run token
	System = "system" // Quai itself: jobs, webhooks, its configuration
)

// Actor is who did what an event records.
type Actor struct {
	Type string
	// ID is the person's name for a user or a token (personal data), the repository for a runner,
	// quai for the system.
	ID string
	// Token is the API token's ID.
	Token string
	// IP is the address the request came from (personal data).
	IP string
	// AuthTime is when the person last authenticated with the identity provider; zero for a token.
	AuthTime time.Time
}

// Object is what an event is about.
type Object struct {
	Type string // request, repository, token, run-token, user
	// ID names it; for a user, their name (personal data).
	ID string
	// Resource is what a request changes.
	Resource string
}

// Event is something to record. Personal values in Details are pseudonymized.
type Event struct {
	Actor   Actor // zero: the context's (WithActor)
	Action  string
	Object  Object
	Details map[string]any
}

// Personal marks a value in an event's details as personal data: a name, an email, what someone
// typed.
type Personal string

// Quai is Quai itself as an actor.
var Quai = Actor{Type: System, ID: "quai"}

// Genesis is the prev of the first event.
var Genesis = strings.Repeat("0", 64)

// PersonalPrefix starts the pseudonym of a personal value in an event.
const PersonalPrefix = "pd:"

type actorKey struct{}

// WithActor returns ctx with who acts in it: the authentication middleware sets the person, a
// run report its repository's runner.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns who acts in ctx: Quai itself when no one was set.
func ActorFrom(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorKey{}).(Actor); ok && a.Type != "" {
		return a
	}
	return Quai
}

type ipKey struct{}

// WithIP returns ctx with the address of the HTTP request it serves.
func WithIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, ipKey{}, ip)
}

// IPFrom returns the address of the HTTP request ctx serves, if any.
func IPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(ipKey{}).(string)
	return ip
}

// Tx is a transaction that records audit events when it commits; store.Tx opens one.
type Tx struct {
	pgx.Tx
	pending []Event
}

// Wrap makes tx record the audit events added to it: call Record just before committing.
func Wrap(tx pgx.Tx) *Tx { return &Tx{Tx: tx} }

// ErrNotRecording is returned by Add for a transaction that was not opened by store.Tx.
var ErrNotRecording = errors.New("audit: the transaction does not record audit events; open it with store.Tx")

// Add adds events to record when tx commits, with the context's actor when they name none.
func Add(ctx context.Context, tx pgx.Tx, events ...Event) error {
	t, ok := tx.(*Tx)
	if !ok {
		return ErrNotRecording
	}
	for _, ev := range events {
		if ev.Actor.Type == "" {
			ev.Actor = ActorFrom(ctx)
		}
		t.pending = append(t.pending, ev)
	}
	return nil
}

// Record writes the events added to the transaction, chained on the last event. It takes the
// audit log's lock until the transaction ends, so it must be the transaction's last statement.
func (t *Tx) Record(ctx context.Context) error {
	if len(t.pending) == 0 {
		return nil
	}
	q := db.New(t.Tx)
	if err := q.LockAudit(ctx); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	seq, prev := int64(0), Genesis
	last, err := q.LastAuditEvent(ctx)
	switch {
	case err == nil:
		seq, prev = last.Seq, last.Hash
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("audit: %w", err)
	}
	at, err := q.AuditClock(ctx)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	for _, ev := range t.pending {
		seq++
		row, personal, err := build(seq, at.Time, ev)
		if err != nil {
			return fmt.Errorf("audit: %s: %w", ev.Action, err)
		}
		row.Prev = prev
		if row.Hash, err = Hash(row); err != nil {
			return fmt.Errorf("audit: %s: %w", ev.Action, err)
		}
		if err := q.InsertAuditEvent(ctx, db.InsertAuditEventParams{Seq: row.Seq, At: row.At, Actor: row.Actor, Action: row.Action,
			Object: row.Object, Details: row.Details, Prev: row.Prev, Hash: row.Hash}); err != nil {
			return fmt.Errorf("audit: %w", err)
		}
		for _, p := range personal {
			if err := q.InsertAuditPersonal(ctx, p); err != nil {
				return fmt.Errorf("audit: %w", err)
			}
		}
		prev = row.Hash
	}
	t.pending = nil
	return nil
}

// build makes an event's row, without prev and hash, and the clear values of its personal data.
func build(seq int64, at time.Time, ev Event) (db.AuditEvent, []db.InsertAuditPersonalParams, error) {
	var personal []db.InsertAuditPersonalParams
	pd := func(v string) string {
		v = clean(v)
		salt := make([]byte, 16)
		_, _ = rand.Read(salt)
		h := sha256.New()
		h.Write(salt)
		h.Write([]byte(v))
		id := PersonalPrefix + hex.EncodeToString(h.Sum(nil))
		personal = append(personal, db.InsertAuditPersonalParams{Hash: id, Seq: seq, Salt: salt, Value: v})
		return id
	}
	a := map[string]any{"type": ev.Actor.Type, "id": clean(ev.Actor.ID)}
	if ev.Actor.Type == User || ev.Actor.Type == Token {
		a["id"] = pd(ev.Actor.ID)
	}
	if ev.Actor.Token != "" {
		a["token"] = clean(ev.Actor.Token)
	}
	if ev.Actor.IP != "" {
		a["ip"] = pd(ev.Actor.IP)
	}
	if !ev.Actor.AuthTime.IsZero() {
		a["authTime"] = ev.Actor.AuthTime.UTC().Format(time.RFC3339)
	}
	o := map[string]any{"type": ev.Object.Type, "id": clean(ev.Object.ID)}
	if ev.Object.Type == "user" {
		o["id"] = pd(ev.Object.ID)
	}
	if ev.Object.Resource != "" {
		o["resource"] = clean(ev.Object.Resource)
	}
	details := map[string]any{}
	for k, v := range ev.Details {
		details[k] = pseudonymize(v, pd)
	}
	actor, err := json.Marshal(a)
	if err != nil {
		return db.AuditEvent{}, nil, err
	}
	object, err := json.Marshal(o)
	if err != nil {
		return db.AuditEvent{}, nil, err
	}
	d, err := json.Marshal(details)
	if err != nil {
		return db.AuditEvent{}, nil, err
	}
	return db.AuditEvent{Seq: seq, At: pgtype.Timestamptz{Time: at, Valid: true}, Actor: actor, Action: ev.Action, Object: object, Details: d}, personal, nil
}

// pseudonymize replaces the personal values in v by their pseudonyms.
func pseudonymize(v any, pd func(string) string) any {
	switch x := v.(type) {
	case Personal:
		if x == "" {
			return ""
		}
		return pd(string(x))
	case []Personal:
		out := make([]string, len(x))
		for i, s := range x {
			out[i] = pd(string(s))
		}
		return out
	case string:
		return clean(x)
	case []string:
		out := make([]string, len(x))
		for i, s := range x {
			out[i] = clean(s)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = pseudonymize(e, pd)
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = pseudonymize(e, pd)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = pseudonymize(e, pd)
		}
		return out
	}
	return v
}

// clean makes s storable in PostgreSQL's text and jsonb: valid UTF-8, without NUL.
func clean(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
}
