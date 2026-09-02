// Command guest is a guardrail used to test the adapter and, through it, to
// run the port's contract suite against a real WASM component.
//
// Its default behaviour is an honest guardrail — it denies payloads carrying
// an AWS key prefix and allows everything else — so the same battery that runs
// against the built-in guardrails and against a sidecar runs against this one.
// The other behaviours exist to prove the adapter refuses the ways a component
// can be wrong.
package main

import (
	"encoding/json"
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

//go:wasmexport handle
func handle(pointer uint32, length uint32) uint64 {
	input := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(pointer))), length)

	var decoded request
	if err := json.Unmarshal(input, &decoded); err != nil {
		return reply(response{Verdict: "deny", Reason: "unreadable request"})
	}
	text := string(decoded.Payload)

	// The behaviour switches on the payload so one build covers every case.
	switch {
	case strings.Contains(text, "BEHAVIOUR:unknown-verdict"):
		return reply(response{Verdict: "perhaps"})
	case strings.Contains(text, "BEHAVIOUR:mutate-without-payload"):
		return reply(response{Verdict: "mutate"})
	case strings.Contains(text, "BEHAVIOUR:payload-on-allow"):
		return reply(response{Verdict: "allow", Payload: []byte("ignored")})
	case strings.Contains(text, "BEHAVIOUR:not-json"):
		return echo("this is not a verdict")
	case strings.Contains(text, "BEHAVIOUR:mutate"):
		return reply(response{
			Verdict: "mutate",
			Payload: []byte(strings.ReplaceAll(text, "BEHAVIOUR:mutate", "[redacted]")),
		})
	case strings.Contains(text, "BEHAVIOUR:echo-phase"):
		return reply(response{Verdict: "deny", Reason: decoded.Phase + "/" + decoded.Model})
	case strings.Contains(text, "AKIA"):
		return reply(response{Verdict: "deny", Reason: "aws-access-key"})
	default:
		return reply(response{Verdict: "allow"})
	}
}

func reply(r response) uint64 {
	encoded, err := json.Marshal(r)
	if err != nil {
		encoded = []byte(`{"verdict":"deny","reason":"internal"}`)
	}
	return emit(encoded)
}

func echo(s string) uint64 { return emit([]byte(s)) }

func emit(out []byte) uint64 {
	keepAlive = append(keepAlive, out)
	return uint64(uintptr(unsafe.Pointer(unsafe.SliceData(out))))<<32 | uint64(len(out))
}

func main() {}
