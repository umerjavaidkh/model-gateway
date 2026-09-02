// Package wasmguardrail runs a guardrail as a WASM module in the worker's own
// process.
//
// The wire shape is the one the sidecar protocol already uses — the same JSON
// request and the same three verdicts — so a publisher moving a component
// between execution modes changes how it is built, not what it says. Two
// encodings for one contract would drift, and the drift would be found by a
// component that behaves differently depending on how it was deployed.
package wasmguardrail

import (
	"context"
	"encoding/json"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wasm"
)

// Guardrail adapts a compiled module to the GuardrailPort. Safe for concurrent
// use: each call gets its own instance.
type Guardrail struct {
	module *wasm.Module
	name   string
}

// New wraps a compiled module.
//
// name is what a snapshot binds to, supplied rather than asked of the module:
// the binding must not depend on what untrusted code says its own name is.
func New(name string, module *wasm.Module) (*Guardrail, error) {
	if name == "" {
		return nil, core.New(core.CodeInvalidRequest, "a WASM guardrail needs a name")
	}
	if module == nil {
		return nil, core.New(core.CodeInvalidRequest, "a WASM guardrail needs a module")
	}
	return &Guardrail{module: module, name: name}, nil
}

// Name reports the component name this guardrail was constructed with.
func (g *Guardrail) Name() string { return g.name }

// Inspect asks the module for a verdict.
func (g *Guardrail) Inspect(
	ctx context.Context, in *core.GuardrailInput,
) (*core.GuardrailResult, error) {
	request, err := json.Marshal(inspectRequest{
		Phase:   phaseName(in.Phase),
		Payload: in.Payload,
		Class:   string(in.Class),
		Model:   in.Meta.Model,
	})
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err, "encoding the guardrail request")
	}

	output, err := g.module.Call(ctx, request)
	if err != nil {
		return nil, err
	}

	var decoded inspectResponse
	if err := json.Unmarshal(output, &decoded); err != nil {
		return nil, core.Wrapf(core.CodeUnavailable, err,
			"decoding the response from module %s", g.name)
	}
	return decoded.toResult(g.name)
}

type inspectRequest struct {
	Phase string `json:"phase"`
	// Payload is base64 in JSON, which is what encoding/json does with []byte.
	// Stated because a guest in another language has to encode it the same
	// way, and "it is a string" is not enough of a specification.
	Payload []byte `json:"payload"`
	Class   string `json:"class"`
	Model   string `json:"model"`
}

type inspectResponse struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	Payload []byte `json:"payload"`
}

func (r inspectResponse) toResult(name string) (*core.GuardrailResult, error) {
	result := &core.GuardrailResult{Reason: r.Reason}

	switch r.Verdict {
	case "allow":
		result.Verdict = core.VerdictAllow
	case "deny":
		result.Verdict = core.VerdictDeny
	case "mutate":
		result.Verdict = core.VerdictMutate
		if r.Payload == nil {
			// The chain would forward the original, so the rewrite silently
			// does not happen.
			return nil, core.Newf(core.CodeUnavailable,
				"module %s returned mutate with no payload", name)
		}
		result.Payload = r.Payload
	default:
		// Not defaulting to allow. An unrecognised verdict means the host and
		// the guest disagree about the protocol, and resolving that in favour
		// of letting the request through is the wrong default for a component
		// whose job is to refuse things.
		return nil, core.Newf(core.CodeUnavailable,
			"module %s returned unknown verdict %q", name, r.Verdict)
	}

	if result.Verdict != core.VerdictMutate && r.Payload != nil {
		return nil, core.Newf(core.CodeUnavailable,
			"module %s returned a payload with a %s verdict", name, r.Verdict)
	}
	return result, nil
}

func phaseName(p core.Phase) string {
	if p == core.PhaseResponse {
		return "response"
	}
	return "request"
}
