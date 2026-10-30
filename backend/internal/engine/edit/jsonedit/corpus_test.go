package jsonedit

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

var update = flag.Bool("update", false, "rewrite the corpus want.json files from the current output")

var corpusDir = filepath.Join("..", "..", "..", "..", "testdata", "corpus", "json")

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

// TestCorpus runs every case in testdata/corpus/json; see the README there.
func TestCorpus(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join(corpusDir, "*", "input.json"))
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no corpus cases in %s (err=%v)", corpusDir, err)
	}
	for _, input := range inputs {
		dir := filepath.Dir(input)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			src := readFile(t, input)
			fragment, refused := readOptional(t, filepath.Join(dir, "want.error"))
			fragment = strings.TrimSpace(fragment)
			doc, err := Decode(src)
			if err != nil {
				if !refused || !strings.Contains(err.Error(), fragment) {
					t.Fatalf("Decode() error = %v", err)
				}
				return
			}
			out, err := Edit(src, corpusIntent(t, doc, readFile(t, filepath.Join(dir, "edit.yaml"))))
			if refused {
				var refusal *RefusalError
				if !errors.As(err, &refusal) || !strings.Contains(err.Error(), fragment) {
					t.Fatalf("Edit() error = %v, want a refusal containing %q", err, fragment)
				}
				return
			}
			if err != nil {
				t.Fatalf("Edit() error = %v", err)
			}
			wantPath := filepath.Join(dir, "want.json")
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
