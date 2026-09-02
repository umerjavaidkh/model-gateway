// Command guest is the misbehaving component the host's tests run against.
//
// One module with several behaviours rather than several modules: compiling a
// guest takes seconds, and the interesting cases are all about what the host
// does when a guest lies to it, not about the guests themselves.
package main

import (
	"strings"
	"unsafe"
)

var keepAlive [][]byte

//go:wasmexport alloc
func alloc(size uint32) uint32 {
	buffer := make([]byte, size)
	keepAlive = append(keepAlive, buffer)
	return uint32(uintptr(unsafe.Pointer(unsafe.SliceData(buffer))))
}

//go:wasmexport handle
func handle(pointer uint32, length uint32) uint64 {
	var mode string
	if length > 0 {
		mode = string(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(pointer))), length))
	}

	switch mode {
	case "loop":
		// Never returns. The host's deadline is the only thing that stops it.
		for {
			keepAlive = keepAlive[:0]
		}
	case "huge":
		// Claims a response far larger than the host permits, without
		// allocating one: the length is the guest's word, and that is the
		// point.
		return uint64(1)<<32 | uint64(1<<30)
	case "out-of-range":
		// A pointer past the end of the guest's own memory.
		return uint64(0xFFFF_FF00)<<32 | uint64(64)
	case "empty":
		return 0
	case "grow":
		// Allocates steadily. The host's memory limit is what stops it.
		for range 4096 {
			keepAlive = append(keepAlive, make([]byte, 1<<20))
		}
		return echo("grew")
	case "stateful":
		// Counts calls in a package variable. If the host reuses instances,
		// this returns a different answer each time.
		calls++
		return echo(strings.Repeat("x", calls))
	default:
		return echo("saw:" + mode)
	}
}

var calls int

func echo(s string) uint64 {
	out := []byte(s)
	keepAlive = append(keepAlive, out)
	return uint64(uintptr(unsafe.Pointer(unsafe.SliceData(out))))<<32 | uint64(len(out))
}

func main() {}
