package evidence

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/store/db"
)

// MaxEvents bounds a request's own events in a bundle: a request has tens.
const MaxEvents = 10000

// Why events are unproven.
const (
	// NoCheckpoint: the log has no checkpoint at all.
	NoCheckpoint = "the audit log has no signed checkpoint: the worker signs one every hour once a signing key is configured " +
		"(audit.checkpoints.signingKey), and until then nothing proves its events"
	// NotYet: the event is after the latest checkpoint.
	NotYet = "no signed checkpoint follows it yet: the worker signs one every hour; download the bundle again after the next"
	// beyond: the segment would pass the bound.
	beyond = "its proof would take %d records of the log, beyond the bundle's bound of %d: verify it with the full log (quai audit export, quai audit verify)"
)

// Audit is a request's audit events, the clear values of their personal data, and their proof.
type Audit struct {
	Events   []audit.Record
	Personal map[string]PersonalValue
	Proof    Proof
}

// Prove reads a request's audit events and, unless omitted says why not, the segments of the log
// that prove them, from q, a snapshot of the log. Segments are the log's records from the
// request's first event before a checkpoint to that checkpoint: they are kept smallest first while
// their records stay within MaxProofRecords.
func Prove(ctx context.Context, q *db.Queries, requestID, omitted string) (Audit, error) {
	rows, err := q.RequestAuditEvents(ctx, db.RequestAuditEventsParams{ID: requestID, MaxRows: MaxEvents + 1})
	if err != nil {
		return Audit{}, err
	}
	if len(rows) > MaxEvents {
		return Audit{}, fmt.Errorf("request %s has more than %d audit events: export the whole log (quai audit export)", requestID, MaxEvents)
	}
	a := Audit{Events: make([]audit.Record, 0, len(rows)), Personal: map[string]PersonalValue{}, Proof: Proof{Segments: []Segment{}, Unproven: []Unproven{}, Bound: MaxProofRecords}}
	seqs := make([]int64, 0, len(rows))
	for _, r := range rows {
		a.Events = append(a.Events, audit.RecordOf(r))
		seqs = append(seqs, r.Seq)
	}
	if err := a.personal(ctx, q, seqs); err != nil {
		return Audit{}, err
	}
	if len(rows) == 0 {
		return a, nil
	}
	if omitted != "" {
		a.Proof.Omitted = omitted
		for _, e := range a.Events {
			a.Proof.Unproven = append(a.Proof.Unproven, Unproven{Seq: e.Seq, Reason: omitted})
		}
		return a, nil
	}
	return a, a.prove(ctx, q)
}

// personal adds the clear values of the events' pseudonyms, those not erased, but the addresses
// people came from.
func (a *Audit) personal(ctx context.Context, q *db.Queries, seqs []int64) error {
	ips := map[string]bool{}
	for _, e := range a.Events {
		var actor struct {
			IP string `json:"ip"`
		}
		if err := json.Unmarshal(e.Actor, &actor); err != nil {
			return fmt.Errorf("event %d: %w", e.Seq, err)
		}
		ips[actor.IP] = true
	}
	if len(seqs) == 0 {
		return nil
	}
	values, err := q.AuditPersonalOf(ctx, seqs)
	if err != nil {
		return err
	}
	for _, v := range values {
		if ips[v.Hash] || !audit.PersonalHolds(v.Hash, v.Salt, v.Value) {
			continue
		}
		a.Personal[v.Hash] = PersonalValue{Value: v.Value, Salt: base64.StdEncoding.EncodeToString(v.Salt)}
	}
	return nil
}

// group is the request's events a checkpoint is the first to follow.
type group struct {
	checkpoint audit.Checkpoint
	seqs       []int64
	cost       int64 // the records of other events the segment takes
}

func (a *Audit) prove(ctx context.Context, q *db.Queries) error {
	cps, err := q.AuditCheckpointsFrom(ctx, a.Events[0].Seq)
	if err != nil {
		return err
	}
	signed := len(cps) > 0
	if !signed {
		if _, err := q.LatestAuditCheckpoint(ctx); err == nil {
			signed = true
		}
	}
	var groups []*group
	i := 0
	for _, e := range a.Events {
		for i < len(cps) && cps[i].Seq < e.Seq {
			i++
		}
		if i == len(cps) {
			reason := NotYet
			if !signed {
				reason = NoCheckpoint
			}
			a.Proof.Unproven = append(a.Proof.Unproven, Unproven{Seq: e.Seq, Reason: reason})
			continue
		}
		if n := len(groups); n > 0 && groups[n-1].checkpoint.Seq == cps[i].Seq {
			groups[n-1].seqs = append(groups[n-1].seqs, e.Seq)
			continue
		}
		groups = append(groups, &group{checkpoint: audit.CheckpointOf(cps[i]), seqs: []int64{e.Seq}})
	}
	for _, g := range groups {
		g.cost = g.checkpoint.Seq - g.seqs[0] + 1 - int64(len(g.seqs))
	}
	byCost := slices.Clone(groups)
	slices.SortStableFunc(byCost, func(x, y *group) int { return cmp.Compare(x.cost, y.cost) })
	total := int64(0)
	kept := map[*group]bool{}
	for _, g := range byCost {
		if total+g.cost > MaxProofRecords {
			continue
		}
		total += g.cost
		kept[g] = true
	}
	for _, g := range groups {
		if !kept[g] {
			for _, s := range g.seqs {
				a.Proof.Unproven = append(a.Proof.Unproven, Unproven{Seq: s, Reason: fmt.Sprintf(beyond, g.cost, MaxProofRecords)})
			}
			continue
		}
		rows, err := q.AuditEventsBetween(ctx, db.AuditEventsBetweenParams{FromSeq: g.seqs[0], ToSeq: g.checkpoint.Seq})
		if err != nil {
			return err
		}
		own := map[int64]bool{}
		for _, s := range g.seqs {
			own[s] = true
		}
		seg := Segment{From: g.seqs[0], To: g.checkpoint.Seq, Checkpoint: g.checkpoint, Records: []audit.Record{}}
		for _, r := range rows {
			if !own[r.Seq] {
				seg.Records = append(seg.Records, audit.RecordOf(r))
			}
		}
		a.Proof.Segments = append(a.Proof.Segments, seg)
	}
	slices.SortFunc(a.Proof.Unproven, func(x, y Unproven) int { return cmp.Compare(x.Seq, y.Seq) })
	return nil
}

// About reports whether a record is about the request id: its object.
func About(r audit.Record, id string) bool {
	var o struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	return json.Unmarshal(r.Object, &o) == nil && o.Type == "request" && o.ID == id
}
