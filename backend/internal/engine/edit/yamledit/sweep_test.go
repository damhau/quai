package yamledit

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestSweep tries every kind of edit on every node of real-world YAML files and reports how
// many succeed and why the others are refused. It runs only when QUAI_YAML_SWEEP lists files or
// directories (separated like PATH):
//
//	QUAI_YAML_SWEEP=$HOME/go/pkg/mod go test -run TestSweep -v ./internal/engine/edit/yamledit
//
// Besides the checks inside Edit, it checks independently that each result decodes to the
// intent, that the same edit leaves it alone, and that changing a one-line scalar to another
// one-line scalar changes at most one line (the original may span several lines, as a folded
// plain or quoted scalar can).
func TestSweep(t *testing.T) {
	roots := os.Getenv("QUAI_YAML_SWEEP")
	if roots == "" {
		t.Skip("QUAI_YAML_SWEEP is not set")
	}
	seen := map[[32]byte]bool{}
	files := map[string]int{}
	edits := map[string]int{}
	refusals := map[string]int{}
	for _, root := range filepath.SplitList(roots) {
		err := filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !(strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")) {
				return nil
			}
			// Test fixtures are edge cases written for parsers, not files people maintain.
			if strings.Contains(name, "/testdata/") || strings.Contains(name, "/test/") {
				return nil
			}
			src, err := os.ReadFile(name)
			if err != nil || len(src) > 200_000 || seen[sha256.Sum256(src)] {
				return nil
			}
			seen[sha256.Sum256(src)] = true
			f, err := parse(src)
			if err != nil {
				files["refused: "+reasonOf(err)]++
				return nil
			}
			files["editable"]++
			ops := sweepEdits(f.value)
			if len(ops) > maxSweepEdits {
				ops = ops[:maxSweepEdits]
			}
			for _, op := range ops {
				out, err := Edit(src, op.want)
				var refusal *RefusalError
				switch {
				case errors.As(err, &refusal) && strings.HasPrefix(refusal.Reason, internalError):
					t.Errorf("%s: %s %s: %v", name, op.kind, op.path, err)
				case errors.As(err, &refusal):
					edits[op.kind+": refused"]++
					refusals[reasonOf(err)]++
					// Refusals by policy (tags, anchors, merge keys, comments in the way) are
					// expected; these point at the editor or the parser.
					if r := reasonOf(err); strings.HasPrefix(r, "verification failed") || strings.Contains(r, "syntax tree") {
						t.Logf("%s: %s %s: %s", name, op.kind, op.path, r)
					}
				case err != nil:
					t.Errorf("%s: %s %s: %v", name, op.kind, op.path, err)
				default:
					edits[op.kind+": ok"]++
					if msg := checkSweep(src, out, op); msg != "" {
						t.Errorf("%s: %s %s: %s", name, op.kind, op.path, msg)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	report(t, "files", files)
	report(t, "edits", edits)
	report(t, "refusal reasons", refusals)
}

// maxSweepEdits bounds the edits tried per file; each one parses the whole file again.
const maxSweepEdits = 400

type sweepEdit struct {
	kind     string
	path     path
	want     any
	oneLiner bool // a one-line scalar replaced by a one-line scalar
}

// sweepEdits returns, for every node: a new scalar of the same type, a removal, an added key
// in each mapping, an appended item in each list, and a mapping in place of each scalar.
func sweepEdits(doc any) []sweepEdit {
	var out []sweepEdit
	for _, p := range nodePaths(doc, path{}, nil) {
		v := valueAt(doc, p)
		switch v := v.(type) {
		case map[string]any:
			out = append(out, sweepEdit{kind: "add key", path: p, want: with(doc, p.key("quai_added"), "x")})
		case []any:
			out = append(out, sweepEdit{kind: "append item", path: p, want: with(doc, p.index(len(v)), "x")})
		default:
			s, single := changedScalar(v)
			out = append(out,
				sweepEdit{kind: "set scalar", path: p, want: with(doc, p, s), oneLiner: single},
				sweepEdit{kind: "scalar to mapping", path: p, want: with(doc, p, map[string]any{"k": "v"})})
		}
		if len(p) > 0 {
			out = append(out, sweepEdit{kind: "remove", path: p, want: without(doc, p)})
		}
	}
	return out
}

// changedScalar returns a different scalar of the same type, and whether both are one-liners.
func changedScalar(v any) (any, bool) {
	switch v := v.(type) {
	case string:
		return v + "-x", !strings.Contains(v, "\n")
	case int64:
		return v + 1, true
	case uint64:
		return v + 1, true
	case float64:
		return v + 1.5, true
	case bool:
		return !v, true
	}
	return "x", true
}

func checkSweep(src, out []byte, op sweepEdit) string {
	got, err := decode(out)
	if err != nil {
		return "result does not decode: " + err.Error()
	}
	if !equal(got, op.want) {
		return "result does not decode to the intent"
	}
	if again, err := Edit(out, op.want); err != nil || string(again) != string(out) {
		return "the same edit changes the result again"
	}
	if op.oneLiner {
		before, after := strings.Split(string(src), "\n"), strings.Split(string(out), "\n")
		if len(after) > len(before) {
			return "a one-line scalar edit added lines"
		}
		if len(before) != len(after) {
			return ""
		}
		changed := 0
		for i := range before {
			if before[i] != after[i] {
				changed++
			}
		}
		if changed != 1 {
			return fmt.Sprintf("a one-line scalar edit changed %d lines", changed)
		}
	}
	return ""
}

// reasonOf shortens a refusal to its reason, without positions or parser details.
func reasonOf(err error) string {
	var refusal *RefusalError
	if !errors.As(err, &refusal) {
		return err.Error()
	}
	reason := refusal.Reason
	for _, cut := range []string{": [", " (yamledit"} {
		if i := strings.Index(reason, cut); i > 0 {
			reason = reason[:i]
		}
	}
	return reason
}

func report(t *testing.T, title string, counts map[string]int) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return counts[b] - counts[a] })
	t.Logf("%s:", title)
	for _, k := range keys {
		t.Logf("  %7d  %s", counts[k], k)
	}
}
