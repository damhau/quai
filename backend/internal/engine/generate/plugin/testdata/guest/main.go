//go:build wasip1

// The plugin the runtime's tests run: it writes a flat mapping of strings as `key = value`
// lines, keeping the comments and the order of the lines it is given. config.mode makes it
// misbehave, for the sandbox's tests: spin, hog, lose, panic, clock, files.
//
// It implements the ABI by hand (docs/design/02, plugin), as a module built with
// GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared.
package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"time"
	"unsafe"
)

// keep holds the buffers the host reads and writes, so that the garbage collector leaves them.
var keep [][]byte

//go:wasmexport alloc
func alloc(size uint32) uint32 {
	b := make([]byte, size)
	keep = append(keep, b)
	return uint32(uintptr(unsafe.Pointer(unsafe.SliceData(b))))
}

func input(ptr, size uint32, v any) error {
	return json.Unmarshal(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), size), v)
}

func output(v any) uint64 {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"error": err.Error()})
	}
	keep = append(keep, b)
	return uint64(uintptr(unsafe.Pointer(unsafe.SliceData(b))))<<32 | uint64(len(b))
}

func fail(err error) uint64 { return output(map[string]string{"error": err.Error()}) }

type config struct {
	Mode string `json:"mode"`
}

//go:wasmexport write
func write(ptr, size uint32) uint64 {
	var in struct {
		Values  map[string]any    `json:"values"`
		Current *string           `json:"current"`
		Params  map[string]string `json:"params"`
		Config  config            `json:"config"`
	}
	if err := input(ptr, size, &in); err != nil {
		return fail(err)
	}
	switch in.Config.Mode {
	case "spin":
		for {
		}
	case "hog":
		var hogs [][]byte
		for {
			hogs = append(hogs, make([]byte, 16<<20))
			keep = append(keep, hogs[len(hogs)-1])
		}
	case "panic":
		panic("the plugin gave up")
	case "lose":
		delete(in.Values, "colour")
	case "clock":
		// Without a clock or randomness, these are the same at every call.
		in.Values["time"] = time.Now().UTC().Format(time.RFC3339Nano)
		in.Values["random"] = fmt.Sprint(rand.Int64())
	case "files":
		_, err := os.ReadFile("/etc/hostname")
		in.Values["files"] = fmt.Sprint(err != nil)
	}
	current := ""
	if in.Current != nil {
		current = *in.Current
	}
	region, err := render(current, in.Values)
	if err != nil {
		return fail(err)
	}
	return output(map[string]string{"region": region})
}

//go:wasmexport read
func read(ptr, size uint32) uint64 {
	var in struct {
		Region string `json:"region"`
	}
	if err := input(ptr, size, &in); err != nil {
		return fail(err)
	}
	values, err := parse(in.Region)
	if err != nil {
		return fail(err)
	}
	return output(map[string]any{"values": values})
}

// render writes values into current: a key's line is rewritten in place, the lines of keys
// values does not hold are removed, and new keys are added at the end, sorted.
func render(current string, values map[string]any) (string, error) {
	var out []string
	seen := map[string]bool{}
	for _, line := range lines(current) {
		key, _, ok := entry(line)
		if !ok {
			out = append(out, line)
			continue
		}
		v, keep := values[key]
		if !keep {
			continue
		}
		s, err := text(key, v)
		if err != nil {
			return "", err
		}
		out = append(out, key+" = "+s)
		seen[key] = true
	}
	var added []string
	for k := range values {
		if !seen[k] {
			added = append(added, k)
		}
	}
	slices.Sort(added)
	for _, k := range added {
		s, err := text(k, values[k])
		if err != nil {
			return "", err
		}
		out = append(out, k+" = "+s)
	}
	if len(out) == 0 {
		return "", nil
	}
	return strings.Join(out, "\n") + "\n", nil
}

func text(key string, v any) (string, error) {
	s, ok := v.(string)
	if !ok || strings.ContainsAny(s, "\n") {
		return "", fmt.Errorf("%s: this format holds single-line strings", key)
	}
	return s, nil
}

func parse(src string) (map[string]any, error) {
	values := map[string]any{}
	for i, line := range lines(src) {
		key, value, ok := entry(line)
		if !ok {
			if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
				return nil, fmt.Errorf("line %d: expected key = value", i+1)
			}
			continue
		}
		values[key] = value
	}
	return values, nil
}

func lines(src string) []string {
	if src == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(src, "\n"), "\n")
}

func entry(line string) (key, value string, ok bool) {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return "", "", false
	}
	k, v, ok := strings.Cut(line, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return "", "", false
	}
	return strings.TrimSpace(k), strings.TrimSpace(v), true
}

func main() {}
