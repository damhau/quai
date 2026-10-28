package requests

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Approvals bound to content (docs/design/05-delivery-and-approvals.md#approvals-bound-to-content):
// after every fetch of a repository, the open pull requests of its requests are reconciled with
// what Git holds now.
//
//   - The base moved, the owned regions unchanged: the branch is rebuilt on the new base with the
//     same changes; same hash, approvals kept.
//   - An owned region changed on the base, or the definitions did: the request is stale.
//   - Someone pushed to the branch: within the request's regions, the request is planned again
//     from the branch (a new change set, approvals void); outside them, it is closed.

// Reconcile reconciles the repository's open pull-request requests with its branches (ingest.Handler).
func (s *Service) Reconcile(ctx context.Context, repo *repositories.Repo) error {
	ds, err := s.store.ListOpenDeliveries(ctx, repo.Name)
	if err != nil {
		return err
	}
	for _, d := range ds {
		r, err := s.get(ctx, d.RequestID)
		if err != nil {
			return err
		}
		if err := s.reconcile(ctx, repo, r, d); err != nil {
			s.log.WarnContext(ctx, "reconciling a pull request", slog.String("request", r.ID.String()), slog.Any("error", err))
		}
	}
	return nil
}

func (s *Service) reconcile(ctx context.Context, repo *repositories.Repo, r db.Request, d db.Delivery) error {
	if d.Head == "" {
		return nil
	}
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil || len(sets) == 0 {
		return err
	}
	cs := sets[0]
	head, err := repo.Mirror.Head(ctx, d.Branch)
	if errors.Is(err, gitx.ErrNotFound) {
		return nil // merged or closed: the pull request's state says
	}
	if err != nil {
		return err
	}
	if head != d.Head {
		if open, err := s.stillOpen(ctx, repo, d); !open {
			return err
		}
		return s.outsidePush(ctx, repo, r, d, cs, head)
	}
	if r.State == Merging {
		return nil // the platform is merging: no rebuild under it
	}
	changed, err := s.definitionsChanged(ctx, r, sets)
	if err != nil {
		return err
	}
	base, err := repo.Mirror.Head(ctx, repo.Branch)
	if err != nil {
		return err
	}
	stale := map[string]any{} // why the request is stale, if it is
	if changed != "" {
		stale["definitions"] = changed
	} else {
		if mb, err := repo.Mirror.MergeBase(ctx, base, head); err != nil || mb == base {
			return err // up to date
		}
		region, err := changedRegion(ctx, repo, cs, base)
		if err != nil {
			return err
		}
		if region != "" {
			stale["region"], stale["base"] = region, base
		}
	}
	if open, err := s.stillOpen(ctx, repo, d); !open {
		return err
	}
	if len(stale) > 0 {
		_, err = s.transition(ctx, r, r.State, Stale, "stale", stale)
		return err
	}
	return s.rebuild(ctx, repo, r, d, cs, base)
}

// stillOpen re-reads the pull request before Quai acts on its branch: one merged or closed
// meanwhile (the base then holds its content) is handled as such instead.
func (s *Service) stillOpen(ctx context.Context, repo *repositories.Repo, d db.Delivery) (bool, error) {
	pr, err := repo.Connector.PullRequests().Get(ctx, d.PullRequest)
	if err != nil {
		return false, err
	}
	if pr.State == connectors.PROpen {
		return true, nil
	}
	return false, s.PullRequest(ctx, repo, pr)
}

// changedRegion returns the first region of the change set whose fingerprint at commit differs
// from the planned one, or "".
func changedRegion(ctx context.Context, repo *repositories.Repo, cs db.ChangeSet, commit string) (string, error) {
	var fps map[string]string
	_ = json.Unmarshal(cs.Fingerprints, &fps)
	files, err := repo.Mirror.Files(ctx, commit)
	if err != nil {
		return "", err
	}
	for _, p := range slices.Sorted(maps.Keys(fps)) {
		strategy := locate.File
		if entries, err := fs.ReadDir(files, p); err == nil && entries != nil {
			strategy = locate.Dir
		}
		now, err := locate.Fingerprint(files, locate.Region{Strategy: strategy, Path: p})
		if err != nil {
			return "", err
		}
		if now != fps[p] {
			return p, nil
		}
	}
	return "", nil
}

// rebuild re-applies the request's changes on the new base and replaces its branch, if the
// branch is still at Quai's head; the hash, and the approvals, stay.
func (s *Service) rebuild(ctx context.Context, repo *repositories.Repo, r db.Request, d db.Delivery, cs db.ChangeSet, base string) error {
	commit, err := s.commit(ctx, r, cs, repo, false)
	if err != nil {
		return err
	}
	commit.Branch, commit.Base, commit.Replace = d.Branch, repo.Branch, d.Head
	sha, err := repo.Mirror.CommitAndPush(ctx, commit)
	if errors.Is(err, gitx.ErrStale) {
		_, err = s.transition(ctx, r, r.State, Stale, "stale", map[string]any{"error": err.Error()})
		return err
	}
	if err != nil {
		return err // someone pushed meanwhile: the next reconcile sees it
	}
	return s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := q.PutDelivery(ctx, db.PutDeliveryParams{RequestID: r.ID, Repository: d.Repository, Branch: d.Branch, PullRequest: d.PullRequest, Url: d.Url, Head: sha}); err != nil {
			return err
		}
		if err := event(ctx, q, r, "rebuilt", "", map[string]any{"base": base, "commit": sha, "hash": cs.Hash}); err != nil {
			return err
		}
		// The platform may reset statuses on a push; an approved request is merged on the new head.
		if err := jobs.InsertTx(ctx, s.jobs, tx, StatusArgs{RequestID: r.ID}, nil); err != nil {
			return err
		}
		if r.State == Approved || r.State == Merging {
			return jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil)
		}
		return nil
	})
}

// outsidePush handles a push to a request's branch by someone else: within the request's
// regions, the request is planned again from the branch; outside them, it is closed.
func (s *Service) outsidePush(ctx context.Context, repo *repositories.Repo, r db.Request, d db.Delivery, cs db.ChangeSet, head string) error {
	base, err := repo.Mirror.Head(ctx, repo.Branch)
	if err != nil {
		return err
	}
	from, err := repo.Mirror.MergeBase(ctx, base, head)
	if err != nil {
		return err
	}
	files, err := repo.Mirror.Diff(ctx, from, head)
	if err != nil {
		return err
	}
	owned, err := s.owned(ctx, cs)
	if err != nil {
		return err
	}
	for _, f := range files {
		if !owned(f.Path) {
			_, err := s.transition(ctx, r, r.State, Cancelled, "modified_outside_quai", map[string]any{"commit": head, "path": f.Path,
				"reason": "the pull request was changed outside Quai, beyond the request's files (" + f.Path + ")"})
			return err
		}
	}
	var changes []plan.Change
	for _, f := range files {
		c := plan.Change{Path: f.Path, Op: plan.OpUpdate}
		if b, err := repo.Mirror.ReadFile(ctx, from, f.Path); err == nil {
			c.Before = b
		}
		switch f.Status {
		case "A":
			c.Op = plan.OpCreate
		case "D":
			c.Op = plan.OpDelete
		}
		if c.Op != plan.OpDelete {
			if c.After, err = repo.Mirror.ReadFile(ctx, head, f.Path); err != nil {
				return err
			}
		}
		changes = append(changes, c)
	}
	slices.SortFunc(changes, func(a, b plan.Change) int { return strings.Compare(a.Path, b.Path) })
	hash := plan.Hash(changes)
	return s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := q.PutDelivery(ctx, db.PutDeliveryParams{RequestID: r.ID, Repository: d.Repository, Branch: d.Branch, PullRequest: d.PullRequest, Url: d.Url, Head: head}); err != nil {
			return err
		}
		if hash == cs.Hash {
			// The same content (a person rebased the branch, say): the approvals stand; the
			// platform reset the statuses on the push, and an approved request merges this head.
			if err := jobs.InsertTx(ctx, s.jobs, tx, StatusArgs{RequestID: r.ID}, nil); err != nil {
				return err
			}
			if r.State == Approved || r.State == Merging {
				return jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil)
			}
			return nil
		}
		if err := q.DeleteChanges(ctx, cs.ID); err != nil {
			return err
		}
		for _, c := range changes {
			if err := q.CreateChange(ctx, db.CreateChangeParams{ChangeSetID: cs.ID, Path: c.Path, Op: c.Op, Before: c.Before, After: c.After}); err != nil {
				return err
			}
		}
		if err := q.SetChangeSetHash(ctx, db.SetChangeSetHashParams{ID: cs.ID, Hash: hash}); err != nil {
			return err
		}
		if r.State != PendingApproval {
			if _, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: r.State, ToState: PendingApproval}); err != nil {
				return conflict(err, r)
			}
		}
		if err := event(ctx, q, r, "modified_outside_quai", "", map[string]any{"commit": head, "hash": hash,
			"reason": "the pull request was changed outside Quai: its approvals are void, approve the new content"}); err != nil {
			return err
		}
		return jobs.InsertTx(ctx, s.jobs, tx, StatusArgs{RequestID: r.ID, Description: "Modified outside Quai: approve the new content in Quai"}, nil)
	})
}

// owned returns whether a path is one of the request's: a file it changes, or inside a region it
// owns.
func (s *Service) owned(ctx context.Context, cs db.ChangeSet) (func(string) bool, error) {
	changes, err := s.store.ListChanges(ctx, cs.ID)
	if err != nil {
		return nil, err
	}
	var fps map[string]string
	_ = json.Unmarshal(cs.Fingerprints, &fps)
	paths := map[string]bool{}
	for _, c := range changes {
		paths[c.Path] = true
	}
	return func(p string) bool {
		if paths[p] {
			return true
		}
		for region := range fps {
			if p == region || strings.HasPrefix(p, strings.TrimSuffix(region, "/")+"/") {
				return true
			}
		}
		return false
	}, nil
}

// alert records something an administrator must look at, on the repository's timeline.
func alert(ctx context.Context, q *db.Queries, repository, kind string, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = q.AddEvent(ctx, db.AddEventParams{RequestID: pgtype.UUID{}, Target: "repository:" + repository, Kind: "alert_" + kind, Data: b})
	return err
}
