package hcledit

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

var update = flag.Bool("update", false, "rewrite the corpus want.hcl files from the current output")

var corpusDir = filepath.Join("..", "..", "..", "..", "testdata", "corpus", "hcl")

// corpusEdit is a case's edit.yaml: the body to edit, and operations applied to it to build the
// intent.
type corpusEdit struct {
	Block string `yaml:"block"`
	Set   []struct {
		Path  []any `yaml:"path"`
		Value any   `yaml:"value"`
	} `yaml:"set"`
	Delete []struct {
		Path []any `yaml:"path"`
	} `yaml:"delete"`
}

// TestCorpus runs every case in testdata/corpus/hcl; see the README there.
func TestCorpus(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join(corpusDir, "*", "input.hcl"))
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no corpus cases in %s (err=%v)", corpusDir, err)
	}
	for _, input := range inputs {
		dir := filepath.Dir(input)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			src := readFile(t, input)
			var e corpusEdit
			if err := yaml.Unmarshal(readFile(t, filepath.Join(dir, "edit.yaml")), &e); err != nil {
				t.Fatalf("edit.yaml: %v", err)
			}
			doc, _, err := Decode(src, e.Block)
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			out, err := Edit(src, e.Block, corpusIntent(t, doc, e))
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
			wantPath := filepath.Join(dir, "want.hcl")
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

func corpusIntent(t *testing.T, doc any, e corpusEdit) any {
	t.Helper()
	for _, s := range e.Set {
		v, err := document.Normalize(s.Value)
		if err != nil {
			t.Fatalf("edit.yaml: %v", err)
		}
		doc = document.With(doc, corpusPath(t, s.Path), v)
	}
	for _, d := range e.Delete {
		doc = document.Without(doc, corpusPath(t, d.Path))
	}
	return doc
}

func corpusPath(t *testing.T, steps []any) document.Path {
	t.Helper()
	path := make(document.Path, len(steps))
	for i, s := range steps {
		switch s := s.(type) {
		case string, int:
			path[i] = s
		default:
			t.Fatalf("edit.yaml: path element %v (%T) is neither a key nor an index", s, s)
		}
	}
	return path
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
