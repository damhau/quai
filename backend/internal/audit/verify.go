package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/store/db"
)

// Where a checkpoint comes from, in what verification says about it.
const (
	FromLog        = "checkpoint"                         // the log's own, audit_checkpoints
	FromRepository = "checkpoint in the audit repository" // an external anchor
)

// ActionPruned is the event pruning records, in the transaction that prunes: the chain starts
// from the checkpoint it names.
const ActionPruned = "audit.pruned"

// Verifier checks the chain as its events come, in seq order, against checkpoints given first:
// every link, every hash, every checkpoint's signature, and that each checkpoint's hash is its
// event's. It names the first seq where the log fails.
//
// The log starts at seq 1 from Genesis, or, once pruned, at a signed checkpoint that an
// audit.pruned event names. A checkpoint below the start covers pruned events and is not
// checked; one beyond the last event means events were lost, such as when a database is
// restored from a backup older than a checkpoint in the audit repository.
type Verifier struct {
	keys   Keys
	points map[int64][]point
	// The chain so far.
	started     bool
	start, next int64
	prev        string
	res         Result
	pruned      map[int64]bool
}

type point struct {
	c    Checkpoint
	from string
	err  error // why its signature does not hold
}

// Result is what a verification found.
type Result struct {
	First, Last int64 // the seqs of the first and the last event read
	Events      int64 // how many events verified
	Checkpoints int   // how many checkpoints matched their event
	Start       int64 // the checkpoint the chain starts from once pruned, 0 from Genesis
	Broken      *Broken
}

// NewVerifier returns a verifier of checkpoints signed by keys.
func NewVerifier(keys Keys) *Verifier {
	return &Verifier{keys: keys, points: map[int64][]point{}, next: 1, prev: Genesis, pruned: map[int64]bool{}}
}

// Checkpoint adds a checkpoint to check, from the log or the audit repository, before the events.
func (v *Verifier) Checkpoint(c Checkpoint, from string) error {
	if v.started {
		return errors.New("a checkpoint after the events: quai audit export writes the checkpoints first")
	}
	for _, p := range v.points[c.Seq] {
		if p.c == c {
			return nil
		}
	}
	v.points[c.Seq] = append(v.points[c.Seq], point{c: c, from: from, err: c.Verify(v.keys)})
	return nil
}

// Done reports whether the log already failed: the events after need not be read.
func (v *Verifier) Done() bool { return v.res.Broken != nil }

// Event checks the next event.
func (v *Verifier) Event(r Record) {
	if v.res.Broken != nil {
		return
	}
	if !v.started {
		v.started = true
		v.res.First = r.Seq
		if r.Seq > 1 && !v.begin(r) {
			return
		}
	}
	v.res.Last = r.Seq
	switch {
	case r.Seq > v.next:
		v.fail(v.next, fmt.Sprintf("the event is missing or out of order (seq %d comes next)", r.Seq))
		return
	case r.Seq < v.next:
		v.fail(r.Seq, "the event comes again or out of order")
		return
	case r.Seq != v.start && r.Prev != v.prev:
		v.fail(r.Seq, "its prev is not the hash of the event before it")
		return
	}
	if h, err := r.Digest(); err != nil {
		v.fail(r.Seq, "it cannot be canonicalized: "+err.Error())
		return
	} else if h != r.Hash {
		v.fail(r.Seq, "its hash is not the hash of its content")
		return
	}
	for _, p := range v.points[r.Seq] {
		switch {
		case p.err != nil:
			v.fail(r.Seq, fmt.Sprintf("the %s does not hold: %v", p.from, p.err))
			return
		case p.c.Hash != r.Hash:
			v.fail(r.Seq, fmt.Sprintf("its hash is not the one the %s signed: the event was changed, or the chain rewritten", p.from))
			return
		}
		v.res.Checkpoints++
	}
	if r.Action == ActionPruned {
		var d struct {
			Checkpoint int64 `json:"checkpoint"`
		}
		if json.Unmarshal(r.Details, &d) == nil {
			v.pruned[d.Checkpoint] = true
		}
	}
	v.res.Events++
	v.next, v.prev = r.Seq+1, r.Hash
}

// begin starts a pruned log from the signed checkpoint of its first event.
func (v *Verifier) begin(r Record) bool {
	for _, p := range v.points[r.Seq] {
		if p.err == nil && p.c.Hash == r.Hash {
			v.start, v.next, v.res.Start = r.Seq, r.Seq, r.Seq
			return true
		}
	}
	// Events before the first are missing: from the last that should be there.
	at := int64(1)
	for seq, ps := range v.points {
		if seq < r.Seq && seq > at && slices.ContainsFunc(ps, func(p point) bool { return p.err == nil }) {
			at = seq
		}
	}
	v.fail(at, fmt.Sprintf("the event is missing: the log starts at seq %d, with no signed checkpoint of it", r.Seq))
	return false
}

func (v *Verifier) fail(seq int64, reason string) {
	v.res.Broken = &Broken{Seq: seq, Reason: reason}
}

// Result returns what the verification found, once every event was read.
func (v *Verifier) Result() Result {
	res := v.res
	if v.start > 0 && !v.pruned[v.start] {
		// Before any later break: the start is the lowest seq read.
		res.Broken = &Broken{Seq: v.start, Reason: fmt.Sprintf("the log starts at the checkpoint at seq %d, but no %s event before any break says it was pruned there: the events before it were deleted", v.start, ActionPruned)}
	}
	if res.Broken != nil {
		return res
	}
	// A checkpoint beyond the last event: events were lost.
	var beyond []int64
	for seq := range v.points {
		if seq > res.Last {
			beyond = append(beyond, seq)
		}
	}
	if len(beyond) > 0 {
		seq := slices.Max(beyond)
		from := v.points[seq][0].from
		reason := fmt.Sprintf("the event is missing: the log ends at seq %d, but a %s says it reached seq %d (a database restored from an older backup?)", res.Last, from, seq)
		if v.points[seq][0].err != nil {
			reason += fmt.Sprintf("; that %s does not hold: %v", from, v.points[seq][0].err)
		}
		res.Broken = &Broken{Seq: res.Last + 1, Reason: reason}
	}
	return res
}

// TxBeginner starts transactions: a pool, or a connection.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// batch is how many events a verification or an export reads at once.
const batch = 1000

// snapshot runs fn in a read-only transaction that sees one state of the log throughout.
func snapshot(ctx context.Context, conn TxBeginner, fn func(q *db.Queries) error) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return fn(db.New(tx))
}

// each calls fn with the log's checkpoints, then with its events in seq order, until stop says.
func each(ctx context.Context, q *db.Queries, checkpoint func(Checkpoint) error, event func(Record) error, stop func() bool) error {
	cps, err := q.AuditCheckpoints(ctx)
	if err != nil {
		return err
	}
	for _, c := range cps {
		if err := checkpoint(CheckpointOf(c)); err != nil {
			return err
		}
	}
	for from := int64(0); !stop(); {
		rows, err := q.AuditEventsFrom(ctx, db.AuditEventsFromParams{Seq: from, MaxRows: batch})
		if err != nil {
			return err
		}
		for _, e := range rows {
			if err := event(RecordOf(e)); err != nil {
				return err
			}
			from = e.Seq + 1
		}
		if len(rows) < batch {
			return nil
		}
	}
	return nil
}

// Verify verifies the log in the database, as it is at one moment, with its checkpoints and the
// anchors from the audit repository: read those first, so that each is in the log already.
func Verify(ctx context.Context, conn TxBeginner, keys Keys, anchors ...Checkpoint) (Result, error) {
	v := NewVerifier(keys)
	for _, a := range anchors {
		if err := v.Checkpoint(a, FromRepository); err != nil {
			return Result{}, err
		}
	}
	err := snapshot(ctx, conn, func(q *db.Queries) error {
		return each(ctx, q,
			func(c Checkpoint) error { return v.Checkpoint(c, FromLog) },
			func(r Record) error { v.Event(r); return nil },
			v.Done)
	})
	if err != nil {
		return Result{}, err
	}
	return v.Result(), nil
}

// Export writes the log as it is at one moment, in JSON lines: its checkpoints, then its events in
// seq order, without the clear values of personal data. It returns how many events it wrote.
func Export(ctx context.Context, conn TxBeginner, w io.Writer) (int64, error) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	var n int64
	err := snapshot(ctx, conn, func(q *db.Queries) error {
		return each(ctx, q,
			func(c Checkpoint) error { return enc.Encode(c) },
			func(r Record) error { n++; return enc.Encode(r) },
			func() bool { return false })
	})
	return n, err
}

// ReadExport feeds an export to a verifier, until the log fails.
func ReadExport(r io.Reader, v *Verifier) error {
	dec := json.NewDecoder(r)
	for line := 1; !v.Done(); line++ {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("value %d: %w", line, err)
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return fmt.Errorf("value %d: not an object: %w", line, err)
		}
		strict := json.NewDecoder(bytes.NewReader(raw))
		strict.DisallowUnknownFields()
		switch probe.Type {
		case CheckpointType:
			var c Checkpoint
			if err := strict.Decode(&c); err != nil {
				return fmt.Errorf("value %d: a checkpoint: %w", line, err)
			}
			if err := v.Checkpoint(c, FromLog); err != nil {
				return fmt.Errorf("value %d: %w", line, err)
			}
		case "":
			var e Record
			if err := strict.Decode(&e); err != nil {
				return fmt.Errorf("value %d: an event: %w", line, err)
			}
			v.Event(e)
		default:
			return fmt.Errorf("value %d: a %q, neither an event nor a checkpoint", line, probe.Type)
		}
	}
	return nil
}
