package repositories

import (
	"context"
	"maps"
	"strings"
)

// ChangeIndex says which commit last changed each file on the history of one head: the
// inventory index of docs/design/01-repository-connections.md#scale, from which resource
// statuses follow (docs/design/06-runs-status-and-drift.md#resource-status). An index is never
// changed once returned.
type ChangeIndex struct {
	Head  string
	files map[string]lastChange
	seq   int // the sequence of the newest commit indexed
}

type lastChange struct {
	commit string
	seq    int // higher is newer
}

// Of returns the commit that last changed path, a file or, for a directory, anything under it;
// empty when nothing did.
func (ix *ChangeIndex) Of(path string) string {
	if ix == nil {
		return ""
	}
	if c, ok := ix.files[path]; ok {
		return c.commit
	}
	var newest lastChange
	prefix := strings.TrimSuffix(path, "/") + "/"
	for f, c := range ix.files {
		if strings.HasPrefix(f, prefix) && c.seq > newest.seq {
			newest = c
		}
	}
	return newest.commit
}

// Changes returns the change index at commit. The index follows the head: when commit descends
// from the head indexed last, only the commits between are read.
func (r *Repo) Changes(ctx context.Context, commit string) (*ChangeIndex, error) {
	r.changesMu.Lock()
	defer r.changesMu.Unlock()
	if r.changes != nil && r.changes.Head == commit {
		return r.changes, nil
	}
	from, base := "", &ChangeIndex{files: map[string]lastChange{}}
	if r.changes != nil {
		if ok, err := r.Mirror.IsAncestor(ctx, r.changes.Head, commit); err == nil && ok {
			from, base = r.changes.Head, r.changes
		}
	}
	commits, err := r.Mirror.Changes(ctx, from, commit)
	if err != nil {
		return nil, err
	}
	// The commits read are newer than any indexed, newest first: the first one seen for a file
	// is its last change.
	ix := &ChangeIndex{Head: commit, files: maps.Clone(base.files), seq: base.seq + len(commits)}
	seen := map[string]bool{}
	for i, c := range commits {
		for _, f := range c.Files {
			if !seen[f] {
				seen[f] = true
				ix.files[f] = lastChange{commit: c.SHA, seq: ix.seq - i}
			}
		}
	}
	r.changes = ix
	return ix, nil
}
