// Package plugintest builds the plugins of the tests from their Go sources, in
// internal/engine/generate/plugin/testdata, as Quai runs them: reactors for WASI preview 1
// (GOOS=wasip1 GOARCH=wasm, -buildmode=c-shared), with the Go toolchain that runs the tests.
// Nothing is downloaded and no module is checked in.
//
// The plugins:
//
//   - guest writes a flat mapping of single-line strings as `key = value` lines, keeping the
//     comments and the order of the lines it is given; config.mode makes it misbehave (spin,
//     hog, lose, panic, clock, files);
//   - forbidden imports env.fetch, a host function plugins are not given.
package plugintest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var (
	mu    sync.Mutex
	built = map[string][]byte{}
)

// Build returns the module built from testdata/<name>, once per test binary.
func Build(t testing.TB, name string) []byte {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if b, ok := built[name]; ok {
		return b
	}
	_, here, _, _ := runtime.Caller(0)
	src := filepath.Join(filepath.Dir(here), "..", "testdata", name)
	out := filepath.Join(t.TempDir(), name+".wasm")
	cmd := exec.Command("go", "build", "-trimpath", "-buildmode=c-shared", "-o", out, ".") // #nosec G204 -- fixed arguments
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the plugin %s: %v\n%s", name, err, b)
	}
	b, err := os.ReadFile(out) // #nosec G304 -- the module just built
	if err != nil {
		t.Fatal(err)
	}
	built[name] = b
	return b
}

// Digest is a module's digest, as a type pins it.
func Digest(module []byte) string {
	sum := sha256.Sum256(module)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Type is a resource type, settings, whose files are settings/{name}.conf, written by the guest
// plugin at module, with an action, paint. Its schema, Schema, is at
// catalog/schemas/settings.schema.json.
func Type(module, digest string) string {
	return fmt.Sprintf(`apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: settings, title: Settings }
spec:
  locate: { strategy: file, path: "settings/{name}.conf" }
  params: [{ name: name, pattern: "[a-z]+" }]
  schema: catalog/schemas/settings.schema.json
  generate:
    kind: plugin
    module: %s
    digest: %s
  ownership: { field: owner }
  deletion: allowed
  actions:
    - name: paint
      inputs: [{ name: colour, type: string, required: true }]
      set: { colour: "${{ action.colour }}" }
`, module, digest)
}

// Schema is the schema of the settings type.
const Schema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["colour"],
  "properties": {
    "owner": { "type": "string" },
    "colour": { "type": "string", "enum": ["blue", "red", "green"] },
    "size": { "type": "string" }
  }
}
`
