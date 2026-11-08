package audit

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/store/db"
)

// SignLatest signs a checkpoint of the latest event and stores it, at now. It returns the
// checkpoint the log holds for that event, which another worker may have signed first, and
// whether there was an event no checkpoint covered yet.
func SignLatest(ctx context.Context, q *db.Queries, key ed25519.PrivateKey, now time.Time) (Checkpoint, bool, error) {
	last, err := q.LastAuditEvent(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, false, nil
	} else if err != nil {
		return Checkpoint{}, false, err
	}
	if c, err := q.LatestAuditCheckpoint(ctx); err == nil && c.Seq >= last.Seq {
		return CheckpointOf(c), false, nil
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, false, err
	}
	c, err := Sign(key, last.Seq, last.Hash, now)
	if err != nil {
		return Checkpoint{}, false, err
	}
	at, err := time.Parse(AtFormat, c.At)
	if err != nil {
		return Checkpoint{}, false, err
	}
	if err := q.InsertAuditCheckpoint(ctx, db.InsertAuditCheckpointParams{
		Seq: c.Seq, Hash: c.Hash, At: pgtype.Timestamptz{Time: at, Valid: true}, Key: c.Key, Signature: c.Signature,
	}); err != nil {
		return Checkpoint{}, false, err
	}
	stored, err := q.AuditCheckpointAt(ctx, c.Seq)
	if err != nil {
		return Checkpoint{}, false, err
	}
	return CheckpointOf(stored), true, nil
}

// Pruned is what pruning deleted: the events before a checkpoint, the log's new start.
type Pruned struct {
	Checkpoint int64
	Events     int64
}

// Prune deletes the events the retention lets go, before the latest checkpoint older than it,
// through the schema owner's audit_prune (open question 19), and records it in tx, which store.Tx
// opened: the chain starts from that checkpoint, and an audit.pruned event says so.
func Prune(ctx context.Context, tx pgx.Tx) (Pruned, error) {
	if _, ok := tx.(*Tx); !ok {
		return Pruned{}, ErrNotRecording
	}
	r, err := db.New(tx).PruneAudit(ctx)
	if err != nil {
		return Pruned{}, err
	}
	p := Pruned{Checkpoint: r.Checkpoint, Events: r.Events}
	if p.Events == 0 {
		return p, nil
	}
	return p, Add(ctx, tx, Event{Actor: Quai, Action: ActionPruned, Object: Object{Type: "audit", ID: "log"},
		Details: map[string]any{"checkpoint": p.Checkpoint, "events": p.Events}})
}
