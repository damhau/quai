// Package plugin runs the modules of the plugin generator (docs/design/02, plugin): WebAssembly
// modules built for WASI preview 1 (GOOS=wasip1), pinned by their SHA-256 digest, run with
// wazero.
//
// A module exports three functions, which exchange JSON through its linear memory:
//
//	alloc(size i32) -> ptr i32       a buffer of size bytes the host writes a call's input into
//	write(ptr, len i32) -> i64       {"values", "current", "params", "config"} -> {"region"}
//	read(ptr, len i32) -> i64        {"region", "params", "config"} -> {"values"}
//
// write and read return the address of their output in the high 32 bits and its length in the
// low 32 bits. The output holds "error" instead when the plugin refuses its input. A region is
// the text of the resource's file; current, absent on create, is its text before the request.
//
// Each call runs in a new instance of the module: nothing is kept from one call to the next.
// The instance has no file system, no network, a fixed clock and a fixed random source, so a
// call's output depends only on its input; it has a memory limit and a time limit. A module may
// import only WASI preview 1, whose functions see that empty world. Compiled modules are cached
// by digest.
//
// The time limit is the one use of the clock in the engine: it can make a call fail, never
// change what a call returns.
package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"github.com/damhau/quai/backend/internal/engine/edit/jsonedit"
)

// The limits of each call, unless New is given others.
const (
	DefaultMemory = 64 << 20 // bytes
	DefaultTime   = 2 * time.Second
)

const pageSize = 64 << 10 // a WebAssembly memory page

var (
	// ErrDigest: the module is not the one the type pins.
	ErrDigest = errors.New("the module's digest is not the one the type pins")
	// ErrImport: the module imports a host function plugins are not given.
	ErrImport = errors.New("the module imports a host function plugins may not use")
	// ErrABI: the module does not export the plugin ABI.
	ErrABI = errors.New("the module does not export the plugin ABI")
	// ErrMemory and ErrTime: a call went over its memory or time limit.
	ErrMemory = errors.New("over its memory limit")
	ErrTime   = errors.New("over its time limit")
	// ErrRefused: the plugin refused the call's input, with its own message.
	ErrRefused = errors.New("refused")
)

// Limits bound each call to a module.
type Limits struct {
	Memory uint32        // bytes of linear memory, rounded down to 64 KiB pages
	Time   time.Duration // wall-clock time, from the module's instantiation to its output
}

// Runtime compiles and runs modules. It is safe for concurrent use.
type Runtime struct {
	limits Limits
	wasm   wazero.Runtime

	mu      sync.Mutex
	modules map[string]*Module // by digest
}

// New returns a runtime whose calls have the given limits; a zero limit is the default.
func New(limits Limits) *Runtime {
	if limits.Memory == 0 {
		limits.Memory = DefaultMemory
	}
	if limits.Time == 0 {
		limits.Time = DefaultTime
	}
	ctx := context.Background()
	wasm := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithMemoryLimitPages(limits.Memory/pageSize).
		WithCloseOnContextDone(true))
	// The functions of WASI preview 1, which every module built for wasip1 imports. Each
	// instance's configuration (call) gives them no file system, a fixed clock and a fixed
	// random source.
	wasi_snapshot_preview1.MustInstantiate(ctx, wasm)
	return &Runtime{limits: limits, wasm: wasm, modules: map[string]*Module{}}
}

// Default is the process's runtime, with the default limits.
var Default = sync.OnceValue(func() *Runtime { return New(Limits{}) })

// Module is a compiled plugin.
type Module struct {
	rt       *Runtime
	path     string
	compiled wazero.CompiledModule
}

// The ABI's exports, with their signatures.
var exports = []struct {
	name            string
	params, results []api.ValueType
}{
	{"alloc", []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}},
	{"write", []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI64}},
	{"read", []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI64}},
}

// Load returns the module at path in fsys, a repository's files, after checking that its
// SHA-256 is digest ("sha256:" and 64 hexadecimal digits), that it imports nothing but WASI
// preview 1 and that it exports the ABI. It is compiled once per digest.
func (rt *Runtime) Load(fsys fs.FS, path, digest string) (*Module, error) {
	src, err := fs.ReadFile(fsys, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("plugin %s does not exist", path)
	}
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", path, err)
	}
	sum := sha256.Sum256(src)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return nil, fmt.Errorf("plugin %s: %w: it is %s, the type pins %s", path, ErrDigest, got, digest)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if m, ok := rt.modules[digest]; ok {
		return &Module{rt: rt, path: path, compiled: m.compiled}, nil
	}
	compiled, err := rt.wasm.CompileModule(context.Background(), src)
	if err != nil {
		return nil, fmt.Errorf("plugin %s is not a WebAssembly module Quai can run: %w", path, err)
	}
	if err := check(compiled); err != nil {
		_ = compiled.Close(context.Background())
		return nil, fmt.Errorf("plugin %s: %w", path, err)
	}
	m := &Module{rt: rt, path: path, compiled: compiled}
	rt.modules[digest] = m
	return m, nil
}

// check refuses a module that imports anything but WASI preview 1, or does not export the ABI.
func check(m wazero.CompiledModule) error {
	for _, f := range m.ImportedFunctions() {
		if mod, name, _ := f.Import(); mod != wasi_snapshot_preview1.ModuleName {
			return fmt.Errorf("%w: %s.%s (a plugin may import only %s)", ErrImport, mod, name, wasi_snapshot_preview1.ModuleName)
		}
	}
	if mems := m.ImportedMemories(); len(mems) > 0 {
		mod, name, _ := mems[0].Import()
		return fmt.Errorf("%w: the memory %s.%s (a plugin has its own memory)", ErrImport, mod, name)
	}
	if _, ok := m.ExportedMemories()["memory"]; !ok {
		return fmt.Errorf("%w: it exports no memory", ErrABI)
	}
	fns := m.ExportedFunctions()
	if _, ok := fns["_start"]; ok {
		return fmt.Errorf("%w: it is a command (it exports _start); build it as a reactor, with -buildmode=c-shared", ErrABI)
	}
	for _, e := range exports {
		f, ok := fns[e.name]
		if !ok || !slices.Equal(f.ParamTypes(), e.params) || !slices.Equal(f.ResultTypes(), e.results) {
			return fmt.Errorf("%w: it must export %s", ErrABI, signature(e.name, e.params, e.results))
		}
	}
	return nil
}

func signature(name string, params, results []api.ValueType) string {
	names := func(ts []api.ValueType) string {
		s := make([]string, len(ts))
		for i, t := range ts {
			s[i] = api.ValueTypeName(t)
		}
		return strings.Join(s, ", ")
	}
	return fmt.Sprintf("%s(%s) -> %s", name, names(params), names(results))
}

// Write returns the region's text with values written into current, its text before the request
// (nil on create). params are the resource's locator params; config is the type's
// spec.generate.config.
func (m *Module) Write(values map[string]any, current []byte, params map[string]string, config map[string]any) ([]byte, error) {
	in := map[string]any{"values": values, "params": params, "config": config}
	if current != nil {
		if !utf8.Valid(current) {
			return nil, fmt.Errorf("plugin %s: the region is not UTF-8 text", m.path)
		}
		in["current"] = string(current)
	}
	var out struct {
		Region *string `json:"region"`
	}
	if err := m.call("write", in, &out); err != nil {
		return nil, err
	}
	if out.Region == nil {
		return nil, fmt.Errorf("plugin %s: write returned no region", m.path)
	}
	return []byte(*out.Region), nil
}

// Read returns the values the region's text holds.
func (m *Module) Read(region []byte, params map[string]string, config map[string]any) (map[string]any, error) {
	if !utf8.Valid(region) {
		return nil, fmt.Errorf("plugin %s: the region is not UTF-8 text", m.path)
	}
	var out struct {
		Values json.RawMessage `json:"values"`
	}
	if err := m.call("read", map[string]any{"region": string(region), "params": params, "config": config}, &out); err != nil {
		return nil, err
	}
	// The values as Quai decodes a JSON file, numbers included.
	v, err := jsonedit.Decode(out.Values)
	values, ok := v.(map[string]any)
	if err != nil || !ok {
		return nil, fmt.Errorf("plugin %s: read returned no mapping of values", m.path)
	}
	return values, nil
}

// call runs fn in a new instance of the module, with in as its input, and decodes its output
// into out.
func (m *Module) call(fn string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("plugin %s: %w", m.path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.rt.limits.Time)
	defer cancel()
	stderr := &capped{max: 4 << 10}
	// No WithFS, WithSysWalltime, WithSysNanotime or WithRandSource: no file system, a clock
	// that starts at a fixed time and only moves when read, and a fixed random source. Args,
	// environment and stdin are empty; stdout is discarded.
	cfg := wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize").WithStderr(stderr)
	mod, err := m.rt.wasm.InstantiateModule(ctx, m.compiled, cfg)
	if err != nil {
		return m.failed(fn, err, stderr)
	}
	defer func() { _ = mod.Close(context.Background()) }()

	res, err := mod.ExportedFunction("alloc").Call(ctx, uint64(len(payload)))
	if err != nil {
		return m.failed(fn, err, stderr)
	}
	ptr := api.DecodeU32(res[0])
	if !mod.Memory().Write(ptr, payload) {
		return fmt.Errorf("plugin %s: alloc returned a buffer outside its memory", m.path)
	}
	res, err = mod.ExportedFunction(fn).Call(ctx, uint64(ptr), uint64(len(payload)))
	if err != nil {
		return m.failed(fn, err, stderr)
	}
	at, size := uint32(res[0]>>32), uint32(res[0])
	view, ok := mod.Memory().Read(at, size)
	if !ok {
		return fmt.Errorf("plugin %s: %s returned an output outside its memory", m.path, fn)
	}
	raw := bytes.Clone(view)
	var refusal struct {
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(raw, &refusal); err != nil {
		return fmt.Errorf("plugin %s: %s returned no JSON object: %w", m.path, fn, err)
	}
	if refusal.Error != nil {
		return fmt.Errorf("plugin %s: %w: %s", m.path, ErrRefused, *refusal.Error)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("plugin %s: %s returned %w", m.path, fn, err)
	}
	return nil
}

// failed describes a call that did not return: over a limit, exited or trapped.
func (m *Module) failed(fn string, err error, stderr *capped) error {
	msg := strings.TrimSpace(stderr.String())
	var exit *sys.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == sys.ExitCodeDeadlineExceeded:
		return fmt.Errorf("plugin %s: %s is %w of %s", m.path, fn, ErrTime, m.rt.limits.Time)
	case strings.Contains(msg, "out of memory"):
		// What the Go runtime says when the memory cannot grow further.
		return fmt.Errorf("plugin %s: %s is %w of %d MiB", m.path, fn, ErrMemory, m.rt.limits.Memory>>20)
	case msg != "":
		return fmt.Errorf("plugin %s: %s failed: %v: %s", m.path, fn, err, msg)
	}
	return fmt.Errorf("plugin %s: %s failed: %w", m.path, fn, err)
}

// capped keeps the first max bytes written to it: what a failing module said on stderr.
type capped struct {
	max int
	buf bytes.Buffer
}

func (t *capped) Write(p []byte) (int, error) {
	if room := t.max - t.buf.Len(); room > 0 {
		t.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (t *capped) String() string { return t.buf.String() }
