// Command wasm-guardrail is a guardrail compiled to WebAssembly, as a
// component publisher would write one.
//
// It is deliberately plain Go: `GOOS=wasip1 GOARCH=wasm go build
// -buildmode=c-shared` is the whole toolchain, so a publisher already writing
// Go needs nothing new to ship an in-process component.
//
// The two exports and their integer pairs are the entire host interface. There
// are no host functions, so there is nothing this module can do but read the
// bytes it was handed and return bytes of its own — it cannot open a file,
// dial a socket, or see anything the host did not copy in.
//
// Build it with `make wasm-example`.
package main

import (
	"encoding/json"
	"strings"
	"unsafe"
)

// request is the same shape the sidecar protocol uses, so a publisher porting
// a component between execution modes changes its build, not its logic.
type request struct {
	Phase   string `json:"phase"`
	Payload []byte `json:"payload"`
	Class   string `json:"class"`
	Model   string `json:"model"`
}

type response struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	Payload []byte `json:"payload,omitempty"`
}

// denied are credential prefixes worth refusing outright. Narrow on purpose: a
// guardrail that fires on anything gets turned off, and a guardrail that is
// turned off protects nothing.
var denied = map[string]string{
	"AKIA":          "aws-access-key",
	"ghp_":          "github-token",
	"-----BEGIN":    "private-key",
	"sk-ant-api03-": "anthropic-key",
}

//go:wasmexport alloc
func alloc(size uint32) uint32 {
	buffer := make([]byte, size)
	// The slice is kept alive by keepAlive below: the host writes into this
	// address before calling handle, and nothing else references it until then.
	keepAlive = append(keepAlive, buffer)
	return uint32(uintptr(unsafe.Pointer(unsafe.SliceData(buffer))))
}

// keepAlive holds buffers handed to the host between alloc and handle.
//
// Without it the collector may free a buffer the host is about to write into,
// and the resulting corruption would be intermittent and blamed on anything
// but this. The instance is destroyed after one call, so it never grows.
var keepAlive [][]byte

//go:wasmexport handle
func handle(pointer uint32, length uint32) uint64 {
	input := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(pointer))), length)

	var decoded request
	if err := json.Unmarshal(input, &decoded); err != nil {
		return reply(response{Verdict: "deny", Reason: "unreadable request"})
	}

	text := string(decoded.Payload)
	for prefix, reason := range denied {
		if strings.Contains(text, prefix) {
			return reply(response{Verdict: "deny", Reason: reason})
		}
	}
	return reply(response{Verdict: "allow"})
}

func reply(r response) uint64 {
	encoded, err := json.Marshal(r)
	if err != nil {
		// Not reachable from the types above, and returning nothing would look
		// to the host like a zero-length response rather than a failure.
		encoded = []byte(`{"verdict":"deny","reason":"internal"}`)
	}
	keepAlive = append(keepAlive, encoded)
	return uint64(uintptr(unsafe.Pointer(unsafe.SliceData(encoded))))<<32 | uint64(len(encoded))
}

// main is required by the toolchain and never runs: a reactor module is
// started through _initialize, not through main.
func main() {}
