//go:build wasip1

// The functions are the module's exports (go:wasmexport): Quai calls them, staticcheck cannot see
// it.
//
//lint:file-ignore U1000 Quai calls the exports
package sdk

import (
	"encoding/json"
	"unsafe"
)

// buffers are the buffers alloc gave Quai to write inputs into, by address, and pinned the
// outputs Quai reads: both stay reachable, so the garbage collector leaves them. A module
// instance serves one call, so nothing is ever released.
var (
	buffers = map[uint32][]byte{}
	pinned  [][]byte
)

func addr(b []byte) uint32 { return uint32(uintptr(unsafe.Pointer(unsafe.SliceData(b)))) }

// alloc returns a buffer of size bytes for the call's input.
//
//go:wasmexport alloc
func alloc(size uint32) uint32 {
	b := make([]byte, size)
	buffers[addr(b)] = b
	return addr(b)
}

//go:wasmexport write
func write(ptr, size uint32) uint64 { return call("write", ptr, size) }

//go:wasmexport read
func read(ptr, size uint32) uint64 { return call("read", ptr, size) }

// call runs fn on the input alloc gave at ptr and returns its output's address (high 32 bits)
// and length.
func call(fn string, ptr, size uint32) uint64 {
	var out []byte
	if input, ok := buffers[ptr]; ok && int(size) <= len(input) {
		out = Handle(registered, fn, input[:size])
	} else {
		out, _ = json.Marshal(map[string]string{"error": "the input is not in a buffer from alloc"})
	}
	pinned = append(pinned, out)
	return uint64(addr(out))<<32 | uint64(len(out))
}
