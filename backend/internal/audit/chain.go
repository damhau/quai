package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gowebpki/jcs"

	"github.com/damhau/quai/backend/internal/store/db"
)

// AtFormat is how an event says when it happened: UTC, to the millisecond.
const AtFormat = "2006-01-02T15:04:05.000Z07:00"

// Record is an event as the log holds it, exports it and verifies it: at as the hash covers it.
type Record struct {
	Seq     int64           `json:"seq"`
	At      string          `json:"at"`
	Actor   json.RawMessage `json:"actor"`
	Action  string          `json:"action"`
	Object  json.RawMessage `json:"object"`
	Details json.RawMessage `json:"details"`
	Prev    string          `json:"prev"`
	Hash    string          `json:"hash"`
}

// RecordOf returns a stored event as a record.
func RecordOf(e db.AuditEvent) Record {
	return Record{Seq: e.Seq, At: Time(e.At.Time), Actor: e.Actor, Action: e.Action, Object: e.Object, Details: e.Details, Prev: e.Prev, Hash: e.Hash}
}

// Canonical returns the event the hash covers, without prev and hash, in RFC 8785's JSON
// Canonicalization Scheme (open question 18).
func (r Record) Canonical() ([]byte, error) {
	b, err := json.Marshal(struct {
		Seq     int64           `json:"seq"`
		At      string          `json:"at"`
		Actor   json.RawMessage `json:"actor"`
		Action  string          `json:"action"`
		Object  json.RawMessage `json:"object"`
		Details json.RawMessage `json:"details"`
	}{r.Seq, r.At, r.Actor, r.Action, r.Object, r.Details})
	if err != nil {
		return nil, err
	}
	return jcs.Transform(b)
}

// Digest returns the hash the event should have: the hex SHA-256 of prev, as its 64 hex
// characters, followed by the canonical event.
func (r Record) Digest() (string, error) {
	c, err := r.Canonical()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(r.Prev))
	h.Write(c)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Canonical returns a stored event in the form its hash covers.
func Canonical(e db.AuditEvent) ([]byte, error) { return RecordOf(e).Canonical() }

// Hash returns the hash a stored event should have.
func Hash(e db.AuditEvent) (string, error) { return RecordOf(e).Digest() }

// Broken is where the chain breaks.
type Broken struct {
	Seq    int64
	Reason string
}

func (b *Broken) Error() string {
	return fmt.Sprintf("the audit log breaks at seq %d: %s", b.Seq, b.Reason)
}

// PersonalHolds reports whether a clear value is the one pseudonymized as hash, with its salt.
func PersonalHolds(hash string, salt []byte, value string) bool {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(value))
	return hash == PersonalPrefix+hex.EncodeToString(h.Sum(nil))
}

// Time formats an event time for the API, as the hash covers it.
func Time(t time.Time) string { return t.UTC().Format(AtFormat) }
