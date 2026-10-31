//go:build wasip1

// A plugin that imports a host function Quai does not give plugins: the runtime refuses it
// before it runs.
package main

import "unsafe"

//go:wasmimport env fetch
func fetch(ptr, size uint32) uint32

var keep [][]byte

//go:wasmexport alloc
func alloc(size uint32) uint32 {
	b := make([]byte, size)
	keep = append(keep, b)
	return uint32(uintptr(unsafe.Pointer(unsafe.SliceData(b))))
}

//go:wasmexport write
func write(ptr, size uint32) uint64 {
	n := fetch(ptr, size)
	return uint64(n)
}

//go:wasmexport read
func read(ptr, size uint32) uint64 { return write(ptr, size) }

func main() {}
