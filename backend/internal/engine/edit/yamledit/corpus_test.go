package yamledit

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

var update = flag.Bool("update", false, "rewrite the corpus want.yaml files from the current output")

// corpusEdit is a case's edit.yaml: operations applied to the decoded input to build the intent.
type corpusEdit struct {
	Set []struct {
		Path  []any `yaml:"path"`
		Value any   `yaml:"value"`
	} `yaml:"set"`
	Delete []struct {
		Path []any `yaml:"path"`
	} `yaml:"delete"`
}

// TestCorpus runs every case in testdata/corpus/yaml; see the README there.
func TestCorpus(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(corpusDir, "*", "input.yaml"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no corpus cases in %s (err=%v)", corpusDir, err)
	}
	for _, input := range dirs {
		dir := filepath.Dir(input)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			src := readFile(t, input)
			intent := corpusIntent(t, mustDecode(t, src), readFile(t, filepath.Join(dir, "edit.yaml")))
			out, err := Edit(src, intent)

			if fragment, ok := readOptional(t, filepath.Join(dir, "want.error")); ok {
				var refusal *RefusalError
				if !errors.As(err, &refusal) || !strings.Contains(err.Error(), strings.TrimSpace(fragment)) {
					t.Fatalf("Edit() error = %v, want a refusal containing %q", err, strings.TrimSpace(fragment))
				}
				return
			}
			if err != nil {
				t.Fatalf("Edit() error = %v", err)
			}
			wantPath := filepath.Join(dir, "want.yaml")
			if *update {
				if err := os.WriteFile(wantPath, out, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if want := readFile(t, wantPath); string(out) != string(want) {
				t.Errorf("Edit() =\n%s\nwant\n%s", out, want)
			}
		})
	}
}

func corpusIntent(t *testing.T, doc any, editSrc []byte) any {
	t.Helper()
	var e corpusEdit
	if err := yaml.Unmarshal(editSrc, &e); err != nil {
		t.Fatalf("edit.yaml: %v", err)
	}
	for _, s := range e.Set {
		v, err := normalize(s.Value, nil)
		if err != nil {
			t.Fatalf("edit.yaml: %v", err)
		}
		doc = with(doc, corpusPath(t, s.Path), v)
	}
	for _, d := range e.Delete {
		doc = without(doc, corpusPath(t, d.Path))
	}
	return doc
}

func corpusPath(t *testing.T, steps []any) path {
	t.Helper()
	p := make(path, len(steps))
	for i, s := range steps {
		switch s := s.(type) {
		case string:
			p[i] = s
		case uint64:
			p[i] = int(s)
		default:
			t.Fatalf("edit.yaml: path element %v (%T) is neither a key nor an index", s, s)
		}
	}
	return p
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readOptional(t *testing.T, name string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}
