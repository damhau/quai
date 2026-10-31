// Package sdktest tests a plugin as Quai uses it, natively with go test: golden cases of requests
// and the files they must write, each read back as Quai reads back every write.
package sdktest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/damhau/quai/plugins/sdk"
)

// Case is a golden case's request.json.
type Case struct {
	// Values are the values to write. Without them, the case reads before.* instead.
	Values map[string]any    `json:"values"`
	Params map[string]string `json:"params"`
	Config map[string]any    `json:"config"`
	// Error, when set, is part of the message the plugin must refuse the case with.
	Error string `json:"error"`
}

// Golden runs the cases under dir, each a directory holding request.json, the file before the
// request as before.<ext> (absent for a create) and the file the request must write as
// after.<ext>. For each case:
//
//   - Write of the values into before must return after, byte for byte;
//   - Read of after must return the values (Quai refuses a write it cannot read back);
//   - Write of what Read returns into after must return after: a file the plugin wrote is
//     rewritten unchanged.
//
// A case whose request.json has error must be refused with that message: by Write when it has
// values, otherwise by Read of before.
func Golden(t *testing.T, p sdk.Plugin, dir string) {
	t.Helper()
	cases, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range cases {
		if !c.IsDir() {
			continue
		}
		n++
		t.Run(c.Name(), func(t *testing.T) { run(t, p, filepath.Join(dir, c.Name())) })
	}
	if n == 0 {
		t.Fatalf("%s holds no case", dir)
	}
}

func run(t *testing.T, p sdk.Plugin, dir string) {
	raw, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c Case
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		t.Fatalf("request.json: %v", err)
	}
	before, hasBefore := file(t, dir, "before.")
	after, hasAfter := file(t, dir, "after.")

	if c.Error != "" {
		var err error
		if c.Values != nil {
			_, err = call[string](p, "write", sdk.WriteRequest{Values: c.Values, Current: ptr(before, hasBefore), Params: c.Params, Config: c.Config})
		} else {
			_, err = call[map[string]any](p, "read", sdk.ReadRequest{Region: before, Params: c.Params, Config: c.Config})
		}
		if err == nil || !strings.Contains(err.Error(), c.Error) {
			t.Fatalf("err = %v, want one with %q", err, c.Error)
		}
		return
	}
	if !hasAfter {
		t.Fatal("the case has no after.* file")
	}
	got, err := Write(p, sdk.WriteRequest{Values: c.Values, Current: ptr(before, hasBefore), Params: c.Params, Config: c.Config})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if got != after {
		t.Fatalf("write =\n%s\nwant\n%s", got, after)
	}
	values, err := Read(p, sdk.ReadRequest{Region: after, Params: c.Params, Config: c.Config})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !same(values, c.Values) {
		t.Fatalf("read = %v, want the values written, %v", values, c.Values)
	}
	again, err := Write(p, sdk.WriteRequest{Values: values, Current: &after, Params: c.Params, Config: c.Config})
	if err != nil {
		t.Fatalf("write of what was read: %v", err)
	}
	if again != after {
		t.Fatalf("write of what was read changes the file:\n%s", again)
	}
}

// Write calls the plugin's Write as Quai does: through the ABI's JSON.
func Write(p sdk.Plugin, r sdk.WriteRequest) (string, error) { return call[string](p, "write", r) }

// Read calls the plugin's Read as Quai does: through the ABI's JSON.
func Read(p sdk.Plugin, r sdk.ReadRequest) (map[string]any, error) {
	return call[map[string]any](p, "read", r)
}

type refusal struct{ msg string }

func (r refusal) Error() string { return r.msg }

func call[T any](p sdk.Plugin, fn string, req any) (T, error) {
	var zero T
	in, err := json.Marshal(req)
	if err != nil {
		return zero, err
	}
	var out struct {
		Region *string        `json:"region"`
		Values map[string]any `json:"values"`
		Error  *string        `json:"error"`
	}
	if err := json.Unmarshal(sdk.Handle(p, fn, in), &out); err != nil {
		return zero, err
	}
	if out.Error != nil {
		return zero, refusal{*out.Error}
	}
	var v any = out.Values
	if fn == "write" {
		if out.Region == nil {
			return zero, refusal{"write returned no region"}
		}
		v = *out.Region
	}
	return v.(T), nil
}

func file(t *testing.T, dir, prefix string) (string, bool) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, prefix+"*"))
	switch len(matches) {
	case 0:
		return "", false
	case 1:
		b, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b), true
	}
	t.Fatalf("%s holds more than one %s* file", dir, prefix)
	return "", false
}

func ptr(s string, ok bool) *string {
	if !ok {
		return nil
	}
	return &s
}

// same compares values as JSON does: numbers by value, whatever their Go type.
func same(a, b map[string]any) bool {
	norm := func(m map[string]any) any {
		if m == nil {
			m = map[string]any{}
		}
		raw, _ := json.Marshal(m)
		var v any
		_ = json.Unmarshal(raw, &v)
		return v
	}
	return reflect.DeepEqual(norm(a), norm(b))
}
