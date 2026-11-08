// Package keeper keeps the audit log (docs/design/09-security-and-compliance.md#audit-log): every
// hour and at shutdown it signs a checkpoint of the latest event and commits it to the audit
// repository, it prunes what the retention lets go, and every day it verifies the chain, its
// checkpoints and the latest checkpoint in the audit repository, recording the outcome in the log
// and in a metric. The worker runs it.
package keeper

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
	"github.com/damhau/quai/backend/internal/telemetry"
)

// The events the keeper records, besides audit.pruned.
const (
	ActionVerified = "audit.verified"
	ActionBroken   = "audit.broken"
)

// Dir is where the audit repository holds checkpoints: Dir/YYYY/MM/DD/<seq>.json, by the day
// they were signed (UTC).
const Dir = "checkpoints"

// Every is how often the keeper signs a checkpoint and prunes, and looks whether a verification
// is due; one is due a day after the last (VerifyEvery, less an interval for the jitter).
const (
	Every       = time.Hour
	VerifyEvery = 24*time.Hour - Every
)

// Options configures a keeper.
type Options struct {
	Store   *store.Store
	Config  config.Audit
	Secrets *secrets.Store
	// Repositories holds the audit repository the configuration names; nil without one.
	Repositories *repositories.Registry
	Committer    gitx.Signature
	Log          *slog.Logger
	Now          func() time.Time // time.Now when nil
}

// Keeper keeps the audit log.
type Keeper struct {
	o Options
}

// New returns a keeper.
func New(o Options) *Keeper {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Keeper{o: o}
}

// Signs reports whether the keeper has a key to sign checkpoints with.
func (k *Keeper) Signs() bool { return k.o.Config.Checkpoints.SigningKey.Ref != "" }

// signingKey resolves the signing key in the secret store.
func (k *Keeper) signingKey(ctx context.Context) (ed25519.PrivateKey, error) {
	sec, err := k.o.Secrets.Resolve(ctx, k.o.Config.Checkpoints.SigningKey.Ref)
	if err != nil {
		return nil, fmt.Errorf("the checkpoint signing key: %w", err)
	}
	return audit.ParsePrivateKey([]byte(sec.Reveal()))
}

// Keys returns the public keys checkpoints verify with: the signing key's, and those of the keys
// before it.
func (k *Keeper) Keys(ctx context.Context) (audit.Keys, error) {
	keys := audit.Keys{}
	for i, pem := range k.o.Config.Checkpoints.PreviousKeys {
		pubs, err := audit.ParsePublicKeys([]byte(pem))
		if err != nil {
			return nil, fmt.Errorf("audit.checkpoints.previousKeys[%d]: %w", i, err)
		}
		keys.Add(pubs...)
	}
	if k.Signs() {
		key, err := k.signingKey(ctx)
		if err != nil {
			return nil, err
		}
		keys.Add(key.Public().(ed25519.PublicKey))
	}
	return keys, nil
}

// Checkpoint signs a checkpoint of the latest event, when no checkpoint covers it yet, and
// commits the latest checkpoint to the audit repository. It returns that checkpoint, none
// without a signing key or an event.
func (k *Keeper) Checkpoint(ctx context.Context) (audit.Checkpoint, error) {
	if !k.Signs() {
		return audit.Checkpoint{}, nil
	}
	key, err := k.signingKey(ctx)
	if err != nil {
		telemetry.AuditCheckpointed(ctx, err)
		return audit.Checkpoint{}, err
	}
	c, fresh, err := audit.SignLatest(ctx, k.o.Store.Queries, key, k.o.Now())
	if err == nil && c.Seq > 0 {
		if fresh {
			k.o.Log.Info("audit checkpoint signed", slog.Int64("seq", c.Seq), slog.String("key", c.Key))
		}
		err = k.commit(ctx, c)
	}
	telemetry.AuditCheckpointed(ctx, err)
	return c, err
}

// repository returns the audit repository, nil when the configuration names none.
func (k *Keeper) repository() (*repositories.Repo, error) {
	name := k.o.Config.Checkpoints.Repository
	if name == "" {
		return nil, nil
	}
	if k.o.Repositories == nil {
		return nil, fmt.Errorf("the audit repository %s: no repository is connected", name)
	}
	repo, err := k.o.Repositories.Get(name)
	if err != nil {
		return nil, fmt.Errorf("the audit repository: %w", err)
	}
	if repo.Delivery != "direct" {
		return nil, fmt.Errorf("the audit repository %s must have delivery mode direct, not %s: Quai commits checkpoints to %s", name, repo.Delivery, repo.Branch)
	}
	return repo, nil
}

// File is where the audit repository holds a checkpoint.
func File(c audit.Checkpoint) (string, error) {
	at, err := time.Parse(audit.AtFormat, c.At)
	if err != nil {
		return "", err
	}
	return path.Join(Dir, at.UTC().Format("2006/01/02"), fmt.Sprintf("%019d.json", c.Seq)), nil
}

// commit commits a checkpoint to the audit repository, if there is one; once is enough.
func (k *Keeper) commit(ctx context.Context, c audit.Checkpoint) error {
	repo, err := k.repository()
	if repo == nil {
		return err
	}
	file, err := File(c)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	sha, err := repo.Mirror.CommitAndPush(ctx, gitx.Commit{
		Branch:  repo.Branch,
		Changes: []plan.Change{{Op: plan.OpCreate, Path: file, After: append(b, '\n')}},
		Message: fmt.Sprintf("Audit checkpoint at seq %d\n\nhash: %s\nkey: %s\n", c.Seq, c.Hash, c.Key),
		Author:  k.o.Committer,
	})
	switch {
	case errors.Is(err, gitx.ErrNoChange):
		return nil
	case err != nil:
		return fmt.Errorf("committing the audit checkpoint at seq %d to %s: %w", c.Seq, repo.Name, err)
	}
	k.o.Log.Info("audit checkpoint committed", slog.Int64("seq", c.Seq), slog.String("repository", repo.Name), slog.String("commit", sha))
	return nil
}

// Anchor returns the checkpoint of the highest seq in the audit repository, as it is now; none
// without an audit repository or a checkpoint in it.
func (k *Keeper) Anchor(ctx context.Context) ([]audit.Checkpoint, error) {
	repo, err := k.repository()
	if repo == nil {
		return nil, err
	}
	if err := repo.Mirror.Fetch(ctx); err != nil {
		return nil, fmt.Errorf("the audit repository %s: %w", repo.Name, err)
	}
	head, err := repo.Mirror.Head(ctx, repo.Branch)
	if err != nil {
		return nil, fmt.Errorf("the audit repository %s: %w", repo.Name, err)
	}
	fsys, err := repo.Mirror.Files(ctx, head)
	if err != nil {
		return nil, err
	}
	latest, seq := "", int64(0)
	err = fs.WalkDir(fsys, Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if n, err := strconv.ParseInt(strings.TrimSuffix(path.Base(p), ".json"), 10, 64); err == nil && n > seq {
			latest, seq = p, n
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) || latest == "" {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("the audit repository %s: %w", repo.Name, err)
	}
	b, err := fs.ReadFile(fsys, latest)
	if err != nil {
		return nil, err
	}
	var c audit.Checkpoint
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("the audit repository %s: %s: %w", repo.Name, latest, err)
	}
	return []audit.Checkpoint{c}, nil
}

// Prune deletes the events the retention lets go, and records it.
func (k *Keeper) Prune(ctx context.Context) (audit.Pruned, error) {
	var p audit.Pruned
	err := k.o.Store.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) (err error) {
		p, err = audit.Prune(ctx, tx)
		return err
	})
	if err == nil && p.Events > 0 {
		k.o.Log.Info("audit log pruned", slog.Int64("events", p.Events), slog.Int64("from", p.Checkpoint))
	}
	return p, err
}

// Verify verifies the chain, its checkpoints and the audit repository's latest checkpoint, and
// records the outcome: an audit.verified or audit.broken event, a metric, and an error in the
// log when it breaks. When the audit repository cannot be read, a chain that holds is not
// recorded as verified: the verification fails, to run again.
func (k *Keeper) Verify(ctx context.Context) (audit.Result, error) {
	var res audit.Result
	keys, err := k.Keys(ctx)
	// The anchor first, then the log: the anchor's event is in the log already.
	anchors, anchorErr := k.Anchor(ctx)
	if anchorErr != nil {
		k.o.Log.Warn("the audit repository cannot be read: verifying without its checkpoint", slog.Any("error", anchorErr))
	}
	if err == nil {
		res, err = audit.Verify(ctx, k.o.Store.Pool, keys, anchors...)
	}
	if err != nil || (anchorErr != nil && res.Broken == nil) {
		telemetry.AuditVerified(ctx, "error")
		return res, errors.Join(err, anchorErr)
	}
	ev := audit.Event{Actor: audit.Quai, Action: ActionVerified, Object: audit.Object{Type: "audit", ID: "log"}, Details: map[string]any{
		"first": res.First, "last": res.Last, "events": res.Events, "checkpoints": res.Checkpoints, "start": res.Start,
	}}
	if b := res.Broken; b != nil {
		ev.Action = ActionBroken
		ev.Details["seq"], ev.Details["reason"] = b.Seq, b.Reason
		k.o.Log.Error("the audit log does not verify: see the runbook in docs/operations/deployment-and-operations.md",
			slog.Int64("seq", b.Seq), slog.String("reason", b.Reason))
		telemetry.AuditVerified(ctx, "broken")
	} else {
		k.o.Log.Info("audit log verified", slog.Int64("events", res.Events), slog.Int("checkpoints", res.Checkpoints))
		telemetry.AuditVerified(ctx, "ok")
	}
	err = k.o.Store.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error { return audit.Add(ctx, tx, ev) })
	return res, err
}

// Due reports whether a verification is due: none recorded in the last VerifyEvery, as the
// database's clock says when it recorded the last one; an event's at is whatever its writer said,
// and a verification dated ahead would put off the next ones.
func (k *Keeper) Due(ctx context.Context) (bool, error) {
	last, err := k.o.Store.LastAuditVerification(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return k.o.Now().Sub(last.Recorded.Time) >= VerifyEvery, nil
}

// CheckpointArgs signs a checkpoint and prunes, every hour.
type CheckpointArgs struct{}

// Kind names the job.
func (CheckpointArgs) Kind() string { return "audit_checkpoint" }

// VerifyArgs verifies the audit log when a verification is due; it runs every hour.
type VerifyArgs struct{}

// Kind names the job.
func (VerifyArgs) Kind() string { return "audit_verify" }

// Workers adds the keeper's workers.
func (k *Keeper) Workers(w *river.Workers) {
	river.AddWorker(w, &checkpointWorker{k: k})
	river.AddWorker(w, &verifyWorker{k: k})
}

// Periodic are the keeper's periodic jobs, for jobs.Options.
func (k *Keeper) Periodic() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(Every), func() (river.JobArgs, *river.InsertOpts) {
			return CheckpointArgs{}, nil
		}, &river.PeriodicJobOpts{RunOnStart: true}),
		river.NewPeriodicJob(river.PeriodicInterval(Every), func() (river.JobArgs, *river.InsertOpts) {
			return VerifyArgs{}, nil
		}, &river.PeriodicJobOpts{RunOnStart: true}),
	}
}

// Shutdown signs a last checkpoint, as the worker stops.
func (k *Keeper) Shutdown(ctx context.Context) error {
	return jobs.WithLock(ctx, k.o.Store.Locks, lockCheckpoint, func(ctx context.Context) error {
		_, err := k.Checkpoint(ctx)
		return err
	})
}

// The keeper's locks: one checkpoint, one verification at a time.
const (
	lockCheckpoint = "quai/audit/checkpoint"
	lockVerify     = "quai/audit/verify"
)

type checkpointWorker struct {
	river.WorkerDefaults[CheckpointArgs]
	k *Keeper
}

// Timeout leaves time for the audit repository's push.
func (w *checkpointWorker) Timeout(*river.Job[CheckpointArgs]) time.Duration { return 5 * time.Minute }

func (w *checkpointWorker) Work(ctx context.Context, _ *river.Job[CheckpointArgs]) error {
	return jobs.WithLock(ctx, w.k.o.Store.Locks, lockCheckpoint, func(ctx context.Context) error {
		// A checkpoint the audit repository did not take does not stop pruning: the log has it.
		_, cerr := w.k.Checkpoint(ctx)
		_, perr := w.k.Prune(ctx)
		return errors.Join(cerr, perr)
	})
}

type verifyWorker struct {
	river.WorkerDefaults[VerifyArgs]
	k *Keeper
}

// Timeout leaves time to read the whole log, below the hour after which a job is rescued.
func (w *verifyWorker) Timeout(*river.Job[VerifyArgs]) time.Duration { return 50 * time.Minute }

func (w *verifyWorker) Work(ctx context.Context, _ *river.Job[VerifyArgs]) error {
	return jobs.WithLock(ctx, w.k.o.Store.Locks, lockVerify, func(ctx context.Context) error {
		if due, err := w.k.Due(ctx); err != nil || !due {
			return err
		}
		_, err := w.k.Verify(ctx)
		return err
	})
}
