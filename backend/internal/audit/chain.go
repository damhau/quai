package audit

import (
	"context"
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

// Canonical returns the event the hash covers, without prev and hash, in RFC 8785's JSON
// Canonicalization Scheme (open question 18).
func Canonical(e db.AuditEvent) ([]byte, error) {
	b, err := json.Marshal(struct {
		Seq     int64           `json:"seq"`
		At      string          `json:"at"`
		Actor   json.RawMessage `json:"actor"`
		Action  string          `json:"action"`
		Object  json.RawMessage `json:"object"`
		Details json.RawMessage `json:"details"`
	}{e.Seq, e.At.Time.UTC().Format(AtFormat), e.Actor, e.Action, e.Object, e.Details})
	if err != nil {
		return nil, err
	}
	return jcs.Transform(b)
}

// Hash returns the event's hash: the hex SHA-256 of prev, as its 64 hex characters, followed by
// the canonical event.
func Hash(e db.AuditEvent) (string, error) {
	c, err := Canonical(e)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(e.Prev))
	h.Write(c)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Broken is where the chain breaks.
type Broken struct {
	Seq    int64
	Reason string
}

func (b *Broken) Error() string {
	return fmt.Sprintf("the audit log breaks at seq %d: %s", b.Seq, b.Reason)
}

// verifyBatch is how many events Verify reads at once.
const verifyBatch = 1000

// Verify walks the chain from its first event: every seq follows the one before, every prev is
// the hash before it, and every hash is the event's. It returns how many events it checked, and
// where the chain breaks first (nil when it holds).
func Verify(ctx context.Context, q *db.Queries) (int64, *Broken, error) {
	var checked int64
	next, prev := int64(1), Genesis
	for {
		rows, err := q.AuditEventsFrom(ctx, db.AuditEventsFromParams{Seq: next, MaxRows: verifyBatch})
		if err != nil {
			return checked, nil, err
		}
		for _, e := range rows {
			if b := check(e, next, prev); b != nil {
				return checked, b, nil
			}
			checked++
			next, prev = e.Seq+1, e.Hash
		}
		if len(rows) < verifyBatch {
			return checked, nil, nil
		}
	}
}

// check verifies one event against the one before it.
func check(e db.AuditEvent, seq int64, prev string) *Broken {
	if e.Seq != seq {
		return &Broken{Seq: seq, Reason: fmt.Sprintf("the event is missing (the next is seq %d)", e.Seq)}
	}
	if e.Prev != prev {
		return &Broken{Seq: e.Seq, Reason: "its prev is not the hash of the event before it"}
	}
	h, err := Hash(e)
	if err != nil {
		return &Broken{Seq: e.Seq, Reason: "it cannot be canonicalized: " + err.Error()}
	}
	if h != e.Hash {
		return &Broken{Seq: e.Seq, Reason: "its hash is not the hash of its content"}
	}
	return nil
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
