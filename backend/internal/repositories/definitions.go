package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Version is a definition as it is active at a commit.
type Version struct {
	Path   string `json:"path"`
	Digest string `json:"digest"` // of its files: the same digest, the same definition
	// Commit is where this version comes from: the set's own commit, or an earlier one when the
	// definition is broken at the set's commit and its last valid version stays active.
	Commit string `json:"commit"`
	Broken bool   `json:"broken,omitempty"`
}

// DefinitionSet is what definitions are active at a commit of a repository
// (docs/design/01-repository-connections.md#definitions).
type DefinitionSet struct {
	Commit     string               `json:"commit"`
	Types      map[string]Version   `json:"types"`
	Blueprints map[string]Version   `json:"blueprints"`
	Problems   definitions.Problems `json:"-"`
}

// active is a set as the database stores it.
type active struct {
	Types      map[string]Version `json:"types"`
	Blueprints map[string]Version `json:"blueprints"`
}

// DefinitionsAt returns the set stored for a commit of a repository; ok is false when none is.
func DefinitionsAt(ctx context.Context, s *store.Store, repository, commit string) (set DefinitionSet, ok bool, err error) {
	row, err := s.GetDefinitionSet(ctx, db.GetDefinitionSetParams{Repository: repository, Commit: commit})
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionSet{}, false, nil
	}
	if err != nil {
		return DefinitionSet{}, false, err
	}
	set, err = decodeSet(row)
	return set, err == nil, err
}

func decodeSet(row db.DefinitionSet) (DefinitionSet, error) {
	var a active
	if err := json.Unmarshal(row.Active, &a); err != nil {
		return DefinitionSet{}, fmt.Errorf("definitions of %s at %.12s: %w", row.Repository, row.Commit, err)
	}
	set := DefinitionSet{Commit: row.Commit, Types: a.Types, Blueprints: a.Blueprints}
	_ = json.Unmarshal(row.Problems, &set.Problems)
	if set.Types == nil {
		set.Types = map[string]Version{}
	}
	if set.Blueprints == nil {
		set.Blueprints = map[string]Version{}
	}
	return set, nil
}

// activate decides what definitions are active at the snapshot's commit, and stores it. A
// definition whose file is broken there keeps the version of the previous set, loaded from the
// commit it comes from. The previous set is the one stored last (the previous snapshot without
// a database). The first process to store a set records what changed as events.
func (repo *Repo) activate(ctx context.Context, commit string, defs *definitions.Repository, problems definitions.Problems, fsys fs.FS) DefinitionSet {
	var prev *DefinitionSet
	if repo.snap != nil {
		prev = &repo.snap.Set
	}
	if repo.store != nil {
		if set, ok, err := DefinitionsAt(ctx, repo.store, repo.Name, commit); err != nil {
			repo.log.WarnContext(ctx, "definitions of the commit cannot be read", slog.String("repository", repo.Name), slog.Any("error", err))
		} else if ok {
			set.Problems = problems
			repo.restore(ctx, set, defs)
			return set
		}
		row, err := repo.store.LatestDefinitionSet(ctx, db.LatestDefinitionSetParams{Repository: repo.Name, Commit: commit})
		switch {
		case err == nil:
			if set, err := decodeSet(row); err == nil {
				prev = &set
			}
		case !errors.Is(err, pgx.ErrNoRows):
			repo.log.WarnContext(ctx, "previous definitions cannot be read", slog.String("repository", repo.Name), slog.Any("error", err))
		}
	}
	set := computeSet(commit, defs, problems, fsys, prev)
	repo.restore(ctx, set, defs)
	if repo.store != nil {
		if err := repo.save(ctx, set, prev); err != nil {
			repo.log.WarnContext(ctx, "definitions cannot be stored", slog.String("repository", repo.Name), slog.Any("error", err))
		}
	}
	return set
}

// computeSet makes the set of a commit: its valid definitions, and those of prev whose file is
// broken at the commit.
func computeSet(commit string, defs *definitions.Repository, problems definitions.Problems, fsys fs.FS, prev *DefinitionSet) DefinitionSet {
	set := DefinitionSet{Commit: commit, Types: map[string]Version{}, Blueprints: map[string]Version{}, Problems: problems}
	for name, t := range defs.Types {
		set.Types[name] = Version{Path: t.Path, Digest: definitions.Digest(fsys, t.Files), Commit: commit}
	}
	for name, b := range defs.Blueprints {
		set.Blueprints[name] = Version{Path: b.Path, Digest: definitions.Digest(fsys, b.Files), Commit: commit}
	}
	if prev == nil {
		return set
	}
	broken := map[string]bool{}
	for _, p := range problems {
		broken[p.Path] = true
	}
	keep := func(now, before map[string]Version) {
		for name, v := range before {
			if _, ok := now[name]; !ok && broken[v.Path] {
				v.Broken = true
				now[name] = v
			}
		}
	}
	keep(set.Types, prev.Types)
	keep(set.Blueprints, prev.Blueprints)
	return set
}

// restore puts into defs the versions the set keeps from earlier commits, read from those
// commits: the catalog and plans then use them as before the broken edit.
func (repo *Repo) restore(ctx context.Context, set DefinitionSet, defs *definitions.Repository) {
	loaded := map[string]*definitions.Repository{}
	at := func(commit string) *definitions.Repository {
		if d, ok := loaded[commit]; ok {
			return d
		}
		fsys, err := repo.Mirror.Files(ctx, commit)
		if err != nil {
			loaded[commit] = nil
			return nil
		}
		d, _ := definitions.Load(fsys)
		loaded[commit] = d
		return d
	}
	for name, v := range set.Types {
		if _, ok := defs.Types[name]; ok || !v.Broken {
			continue
		}
		if d := at(v.Commit); d != nil && d.Types[name] != nil {
			defs.Types[name] = d.Types[name]
		} else {
			repo.log.WarnContext(ctx, "the previous version of a broken type cannot be loaded", slog.String("repository", repo.Name), slog.String("type", name), slog.String("commit", v.Commit))
		}
	}
	for name, v := range set.Blueprints {
		if _, ok := defs.Blueprints[name]; ok || !v.Broken {
			continue
		}
		if d := at(v.Commit); d != nil && d.Blueprints[name] != nil {
			defs.Blueprints[name] = d.Blueprints[name]
		}
	}
}

// save stores the set; the process that stores it first records what changed since prev.
func (repo *Repo) save(ctx context.Context, set DefinitionSet, prev *DefinitionSet) error {
	a, err := json.Marshal(active{Types: set.Types, Blueprints: set.Blueprints})
	if err != nil {
		return err
	}
	problems := set.Problems
	if problems == nil {
		problems = definitions.Problems{}
	}
	p, err := json.Marshal(problems)
	if err != nil {
		return err
	}
	return repo.store.Tx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		n, err := q.PutDefinitionSet(ctx, db.PutDefinitionSetParams{Repository: repo.Name, Commit: set.Commit, Active: a, Problems: p})
		if err != nil || n == 0 || prev == nil {
			return err
		}
		for _, c := range changes(prev, set) {
			author, commit := repo.authorOf(ctx, c.path)
			data, _ := json.Marshal(map[string]any{"repository": repo.Name, "kind": c.kind, "name": c.name, "path": c.path,
				"commit": commit, "setCommit": set.Commit, "problems": problemsOf(set.Problems, c.path)})
			if _, err := q.AddEvent(ctx, db.AddEventParams{RequestID: pgtype.UUID{}, Target: "definitions:" + repo.Name + "/" + c.kind + "/" + c.name,
				Kind: "definition_" + c.change, Actor: author, Data: data}); err != nil {
				return err
			}
		}
		return nil
	})
}

// change is what happened to a definition between two sets.
type change struct {
	kind, name, path, change string // change: added, changed, removed, broken
}

func changes(prev *DefinitionSet, set DefinitionSet) []change {
	var out []change
	diff := func(kind string, before, now map[string]Version) {
		for _, name := range slices.Sorted(maps.Keys(now)) {
			v, old := now[name], before[name]
			_, existed := before[name]
			switch {
			case !existed:
				out = append(out, change{kind, name, v.Path, "added"})
			case v.Broken && !old.Broken:
				out = append(out, change{kind, name, v.Path, "broken"})
			case v.Digest != old.Digest || (old.Broken && !v.Broken):
				out = append(out, change{kind, name, v.Path, "changed"})
			}
		}
		for _, name := range slices.Sorted(maps.Keys(before)) {
			if _, ok := now[name]; !ok {
				out = append(out, change{kind, name, before[name].Path, "removed"})
			}
		}
	}
	diff("type", prev.Types, set.Types)
	diff("blueprint", prev.Blueprints, set.Blueprints)
	return out
}

// authorOf returns who last changed a file on the branch, and in which commit.
func (repo *Repo) authorOf(ctx context.Context, path string) (string, string) {
	log, err := repo.Mirror.Log(ctx, repo.Branch, path, 1)
	if err != nil || len(log) == 0 {
		return "", ""
	}
	return log[0].Author + " <" + log[0].Email + ">", log[0].SHA
}

func problemsOf(all definitions.Problems, path string) []string {
	var out []string
	for _, p := range all {
		if p.Path == path {
			out = append(out, p.String())
		}
	}
	return out
}
