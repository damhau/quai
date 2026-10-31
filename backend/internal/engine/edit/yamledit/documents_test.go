package yamledit

import (
	"errors"
	"strings"
	"testing"
)

// manifestKey addresses a document by kind and metadata.name, as the component generator does.
func manifestKey(doc any) string {
	m, _ := doc.(map[string]any)
	kind, _ := m["kind"].(string)
	meta, _ := m["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if kind == "" || name == "" {
		return ""
	}
	return kind + "/" + name
}

func TestDocuments(t *testing.T) {
	for _, tt := range []struct {
		name, src string
		parts     []string
		values    []any
	}{
		{"empty file", "", nil, nil},
		{"comments only", "# nothing\n", nil, nil},
		{"one document", "a: 1\n", []string{"a: 1\n"}, []any{map[string]any{"a": int64(1)}}},
		{"implicit first", "a: 1\n---\nb: 2\n", []string{"a: 1\n", "---\nb: 2\n"},
			[]any{map[string]any{"a": int64(1)}, map[string]any{"b": int64(2)}}},
		{"comments before the first marker", "# head\n---\na: 1\n---\nb: 2\n", []string{"# head\n---\na: 1\n", "---\nb: 2\n"},
			[]any{map[string]any{"a": int64(1)}, map[string]any{"b": int64(2)}}},
		{"empty documents", "---\n---\nb: 2\n---\n", []string{"---\n", "---\nb: 2\n", "---\n"},
			[]any{nil, map[string]any{"b": int64(2)}, nil}},
		{"content on the marker line", "--- {a: 1}\n--- |\n  x\n", []string{"--- {a: 1}\n", "--- |\n  x\n"},
			[]any{map[string]any{"a": int64(1)}, "x\n"}},
		{"a key that starts like a marker", "a: 1\n---b: 2\n", []string{"a: 1\n---b: 2\n"},
			[]any{map[string]any{"a": int64(1), "---b": int64(2)}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			docs, err := Documents([]byte(tt.src))
			if err != nil {
				t.Fatal(err)
			}
			if len(docs) != len(tt.parts) {
				t.Fatalf("%d documents, want %d: %+v", len(docs), len(tt.parts), docs)
			}
			for i, d := range docs {
				if got := tt.src[d.Start:d.End]; got != tt.parts[i] {
					t.Errorf("document %d = %q, want %q", i, got, tt.parts[i])
				}
				if !equal(d.Value, tt.values[i]) {
					t.Errorf("document %d value = %#v, want %#v", i, d.Value, tt.values[i])
				}
			}
		})
	}
}

func TestDocumentsRefused(t *testing.T) {
	for name, src := range map[string]string{
		"end marker":   "a: 1\n...\n---\nb: 2\n",
		"directive":    "%YAML 1.2\n---\na: 1\n",
		"byte order":   "\xEF\xBB\xBFa: 1\n",
		"not YAML":     "a: [1\n---\nb: 2\n",
		"bare CR line": "a: 1\r---\rb: 2\r",
	} {
		t.Run(name, func(t *testing.T) {
			var re *RefusalError
			if _, err := Documents([]byte(src)); !errors.As(err, &re) {
				t.Errorf("Documents = %v, want a refusal", err)
			}
		})
	}
}

const manifests = `# The team's namespace.
apiVersion: v1
kind: Namespace
metadata:
  name: shop # the namespace
  labels:
    team: shop
---
apiVersion: v1
kind: ResourceQuota
metadata:
  name: quota
  namespace: shop
spec:
  hard:
    requests.cpu: "8"
    requests.memory: 16Gi
`

func ns(labels map[string]any) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "shop", "labels": labels}}
}

func quota(cpu string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "ResourceQuota", "metadata": map[string]any{"name": "quota", "namespace": "shop"},
		"spec": map[string]any{"hard": map[string]any{"requests.cpu": cpu, "requests.memory": "16Gi"}}}
}

var manifestOrder = []string{"apiVersion", "kind", "metadata", "name", "namespace", "labels", "spec"}

func TestEditDocuments(t *testing.T) {
	team := map[string]any{"team": "shop"}
	for _, tt := range []struct {
		name string
		src  string
		want []any
		out  string
	}{
		{"unchanged", manifests, []any{ns(team), quota("8")}, manifests},
		{"one value of the second document", manifests, []any{ns(team), quota("16")},
			strings.Replace(manifests, `requests.cpu: "8"`, `requests.cpu: "16"`, 1)},
		{"addressed by kind and name, not by order", manifests, []any{quota("16"), ns(team)},
			strings.Replace(manifests, `requests.cpu: "8"`, `requests.cpu: "16"`, 1)},
		{"a document removed takes its marker", manifests, []any{ns(team)}, manifests[:strings.Index(manifests, "---")]},
		{"the first document removed", manifests, []any{quota("8")}, manifests[strings.Index(manifests, "---"):]},
		{"a document added at the end", manifests[:strings.Index(manifests, "---")], []any{ns(team), quota("8")},
			manifests[:strings.Index(manifests, "---")] + "---\napiVersion: v1\nkind: ResourceQuota\nmetadata:\n  name: quota\n  namespace: shop\nspec:\n  hard:\n    requests.cpu: \"8\"\n    requests.memory: 16Gi\n"},
		{"into an empty file", "", []any{map[string]any{"a": 1}, map[string]any{"b": 2}}, "a: 1\n---\nb: 2\n"},
		{"after a file without a final line break", "a: 1", []any{map[string]any{"a": 1}, map[string]any{"b": 2}}, "a: 1\n---\nb: 2\n"},
		{"empty documents stay", "---\na: 1 # one\n---\n", []any{map[string]any{"a": 2}}, "---\na: 2 # one\n---\n"},
		{"documents without a key by index", "a: 1\n---\nb: 2 # two\n", []any{map[string]any{"a": 1}, map[string]any{"b": 3}}, "a: 1\n---\nb: 3 # two\n"},
		{"line breaks follow the file", "a: 1\r\n", []any{map[string]any{"a": 1}, map[string]any{"b": 2}}, "a: 1\r\n---\r\nb: 2\r\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := EditDocuments([]byte(tt.src), tt.want, manifestKey, manifestOrder)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tt.out {
				t.Errorf("EditDocuments =\n%s\nwant\n%s", out, tt.out)
			}
		})
	}
}

func TestEditDocumentsRefused(t *testing.T) {
	twice := manifests + "---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: shop\n"
	var re *RefusalError
	if _, err := EditDocuments([]byte(twice), []any{ns(nil)}, manifestKey, nil); !errors.As(err, &re) || !strings.Contains(err.Error(), "Namespace/shop") {
		t.Errorf("a file with a document twice = %v, want a refusal", err)
	}
	if _, err := EditDocuments([]byte(manifests), []any{ns(nil), ns(nil)}, manifestKey, nil); err == nil || errors.As(err, &re) {
		t.Errorf("two documents with one key = %v, want an error", err)
	}
	// An edit inside a document is refused as Edit refuses it.
	anchored := "a: &x 1\nb: *x\n---\nc: 1\n"
	if _, err := EditDocuments([]byte(anchored), []any{map[string]any{"a": 2, "b": 1}, map[string]any{"c": 1}}, manifestKey, nil); !errors.As(err, &re) {
		t.Errorf("an aliased value = %v, want a refusal", err)
	}
}

func TestCreateDocuments(t *testing.T) {
	out, err := CreateDocuments([]any{ns(map[string]any{"team": "shop"}), quota("8")}, manifestOrder)
	if err != nil {
		t.Fatal(err)
	}
	want := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: shop\n  labels:\n    team: shop\n---\n" +
		"apiVersion: v1\nkind: ResourceQuota\nmetadata:\n  name: quota\n  namespace: shop\nspec:\n  hard:\n    requests.cpu: \"8\"\n    requests.memory: 16Gi\n"
	if string(out) != want {
		t.Errorf("CreateDocuments =\n%s\nwant\n%s", out, want)
	}
	// A value cannot add a document.
	out, err = CreateDocuments([]any{map[string]any{"a": "x\n---\nkind: Secret"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if docs, err := Documents(out); err != nil || len(docs) != 1 {
		t.Errorf("a value with a marker = %d documents (%v):\n%s", len(docs), err, out)
	}
}
