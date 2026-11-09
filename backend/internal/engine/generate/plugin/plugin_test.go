package plugin_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/damhau/quai/backend/internal/engine/generate/plugin"
	"github.com/damhau/quai/backend/internal/engine/generate/plugin/plugintest"
)

// modules are the test's plugins, by name.
func modules(t *testing.T) map[string][]byte {
	return map[string][]byte{"guest": plugintest.Build(t, "guest"), "forbidden": plugintest.Build(t, "forbidden")}
}

func digest(b []byte) string { return plugintest.Digest(b) }

// repo is a repository holding the test's plugins under .quai/plugins.
func repo(t *testing.T) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, b := range modules(t) {
		fsys[".quai/plugins/"+name+".wasm"] = &fstest.MapFile{Data: b}
	}
	return fsys
}

func load(t *testing.T, rt *plugin.Runtime, name string) *plugin.Module {
	t.Helper()
	m, err := rt.Load(repo(t), ".quai/plugins/"+name+".wasm", digest(modules(t)[name]))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestWriteRead: a plugin writes values into a file, keeping what it does not manage, and reads
// them back; each call starts afresh and calls run concurrently.
func TestWriteRead(t *testing.T) {
	m := load(t, plugin.Default(), "guest")
	params := map[string]string{"name": "web"}

	created, err := m.Write(map[string]any{"name": "web", "colour": "blue"}, nil, params, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(created) != "colour = blue\nname = web\n" {
		t.Errorf("created = %q", created)
	}

	current := []byte("# Managed by Quai.\nname = web\ncolour = blue # not a comment\nsize = big\n")
	updated, err := m.Write(map[string]any{"name": "web", "colour": "red", "owner": "alice"}, current, params, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# Managed by Quai.\nname = web\ncolour = red\nowner = alice\n"; string(updated) != want {
		t.Errorf("updated = %q, want %q", updated, want)
	}
	values, err := m.Read(updated, params, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(values) != "map[colour:red name:web owner:alice]" {
		t.Errorf("read = %v", values)
	}

	// The plugin's own refusals are errors, with its message.
	if _, err := m.Read([]byte("no equals sign\n"), params, nil); !errors.Is(err, plugin.ErrRefused) || !strings.Contains(err.Error(), "line 1: expected key = value") {
		t.Errorf("read of a broken file: err = %v", err)
	}
	if _, err := m.Write(map[string]any{"size": 3}, nil, params, nil); !errors.Is(err, plugin.ErrRefused) || !strings.Contains(err.Error(), "size: this format holds single-line strings") {
		t.Errorf("write of a number: err = %v", err)
	}
	if _, err := m.Read([]byte{0xff, 0xfe}, params, nil); err == nil || !strings.Contains(err.Error(), "not UTF-8") {
		t.Errorf("read of bytes that are not text: err = %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			v := fmt.Sprint(i)
			out, err := m.Write(map[string]any{"n": v}, nil, nil, nil)
			if err == nil && string(out) != "n = "+v+"\n" {
				err = fmt.Errorf("call %d wrote %q", i, out)
			}
			errs[i] = err
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Errorf("concurrent calls: %v", err)
	}
}

// TestSandbox: a call sees no file system, clock or randomness, so the same input gives the same
// output; a call over its memory or time limit, a panic and a lost value are errors.
func TestSandbox(t *testing.T) {
	rt := plugin.New(plugin.Limits{Memory: 32 << 20, Time: 500 * time.Millisecond})
	m := load(t, rt, "guest")
	mode := func(s string) map[string]any { return map[string]any{"mode": s} }

	first, err := m.Write(map[string]any{}, nil, nil, mode("clock"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Write(map[string]any{}, nil, nil, mode("clock"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("the clock or the random source moved between calls:\n%s\n%s", first, second)
	}
	if out, err := m.Write(map[string]any{}, nil, nil, mode("files")); err != nil || string(out) != "files = true\n" {
		t.Errorf("a plugin read a file: %q, %v", out, err)
	}

	start := time.Now()
	if _, err := m.Write(map[string]any{}, nil, nil, mode("spin")); !errors.Is(err, plugin.ErrTime) || !strings.Contains(err.Error(), "write is over its time limit of 500ms") {
		t.Errorf("an endless loop: err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("an endless loop ran for %s", d)
	}
	// Allocating 32 MiB can take longer than 500ms on a slow machine (backend CI, 2026-11-09): the
	// memory limit is checked on a runtime whose time limit cannot come first.
	hog := load(t, plugin.New(plugin.Limits{Memory: 32 << 20, Time: time.Minute}), "guest")
	if _, err := hog.Write(map[string]any{}, nil, nil, mode("hog")); !errors.Is(err, plugin.ErrMemory) || !strings.Contains(err.Error(), "write is over its memory limit of 32 MiB") {
		t.Errorf("a call that allocates without end: err = %v", err)
	}
	if _, err := m.Write(map[string]any{}, nil, nil, mode("panic")); err == nil || !strings.Contains(err.Error(), "panic: the plugin gave up") {
		t.Errorf("a panic: err = %v", err)
	}
	// The module still runs after its calls failed: each call has its own instance.
	if out, err := m.Write(map[string]any{"a": "b"}, nil, nil, nil); err != nil || string(out) != "a = b\n" {
		t.Errorf("after the failures: %q, %v", out, err)
	}
}

// TestRefused: a module is refused before it runs when its digest differs from the type's, it
// imports a host function plugins are not given, it does not export the ABI, or it is missing.
func TestRefused(t *testing.T) {
	rt := plugin.Default()
	fsys := repo(t)
	fsys[".quai/plugins/empty.wasm"] = &fstest.MapFile{Data: []byte("\x00asm\x01\x00\x00\x00")}
	fsys[".quai/plugins/text.wasm"] = &fstest.MapFile{Data: []byte("not a module")}

	other := digest([]byte("another module"))
	if _, err := rt.Load(fsys, ".quai/plugins/guest.wasm", other); !errors.Is(err, plugin.ErrDigest) || !strings.Contains(err.Error(), "the type pins "+other) {
		t.Errorf("a wrong digest: err = %v", err)
	}
	if _, err := rt.Load(fsys, ".quai/plugins/forbidden.wasm", digest(modules(t)["forbidden"])); !errors.Is(err, plugin.ErrImport) || !strings.Contains(err.Error(), "env.fetch") {
		t.Errorf("an import of env.fetch: err = %v", err)
	}
	if _, err := rt.Load(fsys, ".quai/plugins/empty.wasm", digest(fsys[".quai/plugins/empty.wasm"].Data)); !errors.Is(err, plugin.ErrABI) {
		t.Errorf("a module without the ABI: err = %v", err)
	}
	if _, err := rt.Load(fsys, ".quai/plugins/text.wasm", digest(fsys[".quai/plugins/text.wasm"].Data)); err == nil || !strings.Contains(err.Error(), "is not a WebAssembly module") {
		t.Errorf("a file that is not a module: err = %v", err)
	}
	if _, err := rt.Load(fsys, ".quai/plugins/missing.wasm", other); err == nil || err.Error() != "plugin .quai/plugins/missing.wasm does not exist" {
		t.Errorf("a missing module: err = %v", err)
	}
}

// TestExamples: the plugins of the repository's plugins directory, built with its SDK, run their
// golden cases in the runtime as they do natively (plugins/sdk/sdktest): each write gives its
// after file, which reads back as the values, and each refusal is ErrRefused with its message.
func TestExamples(t *testing.T) {
	for _, name := range []string{"ini", "template"} {
		t.Run(name, func(t *testing.T) {
			b := plugintest.Example(t, name)
			m, err := plugin.Default().Load(fstest.MapFS{"p.wasm": {Data: b}}, "p.wasm", digest(b))
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join("..", "..", "..", "..", "..", "plugins", name, "testdata")
			cases, err := os.ReadDir(dir)
			if err != nil || len(cases) == 0 {
				t.Fatalf("the cases of %s: %v", name, err)
			}
			for _, c := range cases {
				t.Run(c.Name(), func(t *testing.T) { example(t, m, filepath.Join(dir, c.Name())) })
			}
		})
	}
}

func example(t *testing.T, m *plugin.Module, dir string) {
	var c struct {
		Values map[string]any    `json:"values"`
		Params map[string]string `json:"params"`
		Config map[string]any    `json:"config"`
		Error  string            `json:"error"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "request.json")) // #nosec G304 -- the test's cases
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	read := func(prefix string) []byte {
		matches, _ := filepath.Glob(filepath.Join(dir, prefix+"*"))
		if len(matches) == 0 {
			return nil
		}
		b, err := os.ReadFile(matches[0]) // #nosec G304 -- the test's cases
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	before, after := read("before."), read("after.")
	if c.Error != "" {
		if c.Values != nil {
			_, err = m.Write(c.Values, before, c.Params, c.Config)
		} else {
			_, err = m.Read(before, c.Params, c.Config)
		}
		if !errors.Is(err, plugin.ErrRefused) || !strings.Contains(err.Error(), c.Error) {
			t.Fatalf("err = %v, want a refusal with %q", err, c.Error)
		}
		return
	}
	got, err := m.Write(c.Values, before, c.Params, c.Config)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(after) {
		t.Fatalf("write =\n%s\nwant\n%s", got, after)
	}
	values, err := m.Read(after, c.Params, c.Config)
	if err != nil {
		t.Fatal(err)
	}
	if g, w := asJSON(t, values), asJSON(t, c.Values); g != w {
		t.Errorf("read = %s, want %s", g, w)
	}
}

func asJSON(t *testing.T, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
