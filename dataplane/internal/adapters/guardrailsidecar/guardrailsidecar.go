// Package guardrailsidecar speaks to an out-of-process guardrail over a Unix
// socket.
//
// This is the sidecar protocol for the component registry: how a guardrail that
// is not written in Go, and not compiled into the worker, is driven as a
// core.GuardrailPort. It is what the admission runner points at a sandboxed
// component, and what a worker would use to run one in production.
//
// JSON over a Unix socket, matching the NER sidecar rather than introducing
// gRPC for one more thing. The reasoning is the same and is recorded in ADR
// 0008: this crosses a language boundary between two processes deployed
// together, not a version boundary, and being able to curl the socket at three
// in the morning is worth more than the wire efficiency.
package guardrailsidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// DefaultTimeout bounds one call when the caller sets none.
//
// The chain enforces the binding's own timeout on top of this, and that one is
// authoritative. This only stops a call outliving the process that made it.
const DefaultTimeout = 5 * time.Second

// MaxResponseBytes caps what a component can return.
//
// A mutate verdict returns a payload, so the response is attacker-influenced in
// size. Without a cap, a component under test could answer one small request
// with gigabytes and take the runner down instead of failing its suite.
const MaxResponseBytes = 8 << 20

// Guardrail is a client for one sidecar. Safe for concurrent use.
type Guardrail struct {
	client *http.Client
	name   string
}

// Option configures a Guardrail.
type Option func(*Guardrail)

// WithTimeout bounds a single call.
func WithTimeout(d time.Duration) Option {
	return func(g *Guardrail) {
		if d > 0 {
			g.client.Timeout = d
		}
	}
}

// New returns a client for the guardrail listening on socketPath.
//
// name is what a snapshot binds to. It is supplied rather than asked of the
// component, because the binding must not depend on what an untrusted process
// says its own name is.
func New(name, socketPath string, opts ...Option) (*Guardrail, error) {
	if name == "" {
		return nil, core.New(core.CodeInvalidRequest, "a guardrail sidecar needs a name")
	}
	if socketPath == "" {
		return nil, core.New(core.CodeInvalidRequest, "a guardrail sidecar needs a socket path")
	}

	dialer := &net.Dialer{}
	guardrail := &Guardrail{
		name: name,
		client: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socketPath)
				},
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
	for _, opt := range opts {
		opt(guardrail)
	}
	return guardrail, nil
}

// Name reports the component name this client was constructed with.
func (g *Guardrail) Name() string { return g.name }

// Ping checks the sidecar is answering, for use at startup.
func (g *Guardrail) Ping(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://guardrail/healthz", nil)
	if err != nil {
		return core.Wrap(core.CodeInternal, err, "building the health request")
	}
	response, err := g.client.Do(request)
	if err != nil {
		return core.Wrap(core.CodeUnavailable, err, "the guardrail sidecar is not answering")
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))

	if response.StatusCode != http.StatusOK {
		return core.Newf(core.CodeUnavailable,
			"the guardrail sidecar reported %d", response.StatusCode)
	}
	return nil
}

// Inspect asks the sidecar for a verdict.
func (g *Guardrail) Inspect(
	ctx context.Context, in *core.GuardrailInput,
) (*core.GuardrailResult, error) {
	body, err := json.Marshal(inspectRequest{
		Phase:   phaseName(in.Phase),
		Payload: in.Payload,
		Class:   string(in.Class),
		Model:   in.Meta.Model,
	})
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err, "encoding the guardrail request")
	}

	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, "http://guardrail/v1/inspect", bytes.NewReader(body))
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err, "building the guardrail request")
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := g.client.Do(request)
	if err != nil {
		return nil, core.Wrap(core.CodeUnavailable, err, "calling the guardrail sidecar")
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, core.Newf(core.CodeUnavailable,
			"the guardrail sidecar reported %d", response.StatusCode)
	}

	var decoded inspectResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, MaxResponseBytes)).Decode(&decoded); err != nil {
		return nil, core.Wrap(core.CodeUnavailable, err, "decoding the guardrail response")
	}
	return decoded.toResult()
}

type inspectRequest struct {
	Phase string `json:"phase"`
	// Payload is base64 in JSON, which is what encoding/json does with []byte.
	// Said explicitly because a component in another language has to encode it
	// the same way, and "it is a string" is not enough of a spec.
	Payload []byte `json:"payload"`
	Class   string `json:"class"`
	Model   string `json:"model"`
}

type inspectResponse struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	Payload []byte `json:"payload"`
}

func (r inspectResponse) toResult() (*core.GuardrailResult, error) {
	result := &core.GuardrailResult{Reason: r.Reason}

	switch r.Verdict {
	case "allow":
		result.Verdict = core.VerdictAllow
	case "deny":
		result.Verdict = core.VerdictDeny
	case "mutate":
		result.Verdict = core.VerdictMutate
		if r.Payload == nil {
			// The chain would forward the original, so a mutate with no
			// payload is a rewrite that silently does not happen.
			return nil, core.New(core.CodeUnavailable,
				"the guardrail returned mutate with no payload")
		}
		result.Payload = r.Payload
	default:
		// Not defaulting to allow. An unrecognised verdict means the two sides
		// disagree about the protocol, and resolving that disagreement in
		// favour of letting the request through is the wrong default for a
		// component whose job is to refuse things.
		return nil, core.Newf(core.CodeUnavailable, "unknown verdict %q", r.Verdict)
	}

	if result.Verdict != core.VerdictMutate && r.Payload != nil {
		// Returned but never applied. Refusing is louder than ignoring, and
		// this is exactly the mistake the contract suite exists to catch.
		return nil, core.Newf(core.CodeUnavailable,
			"the guardrail returned a payload with a %s verdict", r.Verdict)
	}
	return result, nil
}

func phaseName(p core.Phase) string {
	if p == core.PhaseResponse {
		return "response"
	}
	return "request"
}
