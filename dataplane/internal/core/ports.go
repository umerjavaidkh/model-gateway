package core

import (
	"context"
	"time"
)

// PortName identifies an extension point a registry component can fill.
type PortName string

// The four data-plane ports. See the note below on why there is no fifth.
const (
	PortProvider  PortName = "provider"
	PortGuardrail PortName = "guardrail"
	PortStore     PortName = "store"
	PortTelemetry PortName = "telemetry"
)

// These four ports are the entire data-plane extension surface, and the count is
// a deliberate ceiling rather than a starting point. Every port is a contract
// maintained forever and a compatibility surface for every component that fills
// it. If something new sits in the request path and does not fit these four, the
// default answer is that it belongs in the core, not behind a fifth port.
//
// Control-plane ports (secrets, trainers, eval suites) are a separate set with a
// separate discipline: their work is asynchronous and artifact-producing, their
// latency budget is minutes rather than milliseconds, and they never execute
// inside a request. They are defined by the control plane, not here.

// ---------------------------------------------------------------------------
// ProviderPort
// ---------------------------------------------------------------------------

// Credential is a resolved provider secret. It reaches a ProviderPort as a call
// argument and is never stored on the adapter, so that credential rotation does
// not require reconstructing adapters.
type Credential struct {
	Ref    string
	Secret []byte
}

// ProviderCall is one attempt against one deployment.
type ProviderCall struct {
	Deployment Deployment
	Meta       RequestMeta
	Body       []byte
	Credential Credential
}

// TokenUsage is what the provider reported it consumed, broken down by billing
// class. Providers that do not report usage leave this zero, and the accounting
// layer estimates instead.
//
// The classes are normalized by each adapter, because providers disagree about
// whether cached tokens are counted inside the input total or alongside it.
// Here they are always disjoint: Input excludes CachedInput and CacheWrite, so
// Total is a plain sum and no caller has to know which convention a provider
// used.
type TokenUsage struct {
	// Input is standard, uncached input.
	Input int64
	// CachedInput was served from the provider's prompt cache, and is billed at
	// a fraction of the standard rate.
	CachedInput int64
	// CacheWrite was written into the provider's cache, and costs more than a
	// plain read.
	CacheWrite int64
	Output     int64
}

// Total is every token the request consumed, of any class.
func (u TokenUsage) Total() int64 {
	return u.Input + u.CachedInput + u.CacheWrite + u.Output
}

// TotalInput is every input token, of any class. Useful where the distinction
// does not matter, such as a rate limit expressed in plain tokens.
func (u TokenUsage) TotalInput() int64 {
	return u.Input + u.CachedInput + u.CacheWrite
}

// ProviderResponse is a completed non-streaming call.
type ProviderResponse struct {
	StatusCode int
	Body       []byte
	Usage      TokenUsage
}

// Chunk is one normalized piece of a streaming response. Body carries the
// already-translated bytes to relay to the caller; Usage is populated on the
// final chunk by providers that report it.
type Chunk struct {
	Body  []byte
	Usage TokenUsage
	Final bool
}

// ChunkStream is a pull-based cursor over a streaming response.
//
// A channel would be more idiomatic for producers, but a cursor is better here
// because the response leg is a pipeline: PII restoration needs a rolling buffer
// across chunks, token counting needs to observe every chunk, and both need to
// propagate an error mid-stream and release the upstream connection
// deterministically. Next-plus-Close expresses all three; a channel expresses
// none of them without a side band.
type ChunkStream interface {
	// Next returns the next chunk. It returns io.EOF when the stream is complete.
	Next(ctx context.Context) (Chunk, error)
	// Close releases the upstream connection. It is safe to call more than once
	// and must be called even when Next returned io.EOF.
	Close() error
}

// ProviderPort translates a gateway request into an upstream call and normalizes
// what comes back. It is the port with the most implementations and the least
// gateway-specific logic: everything vendor-shaped lives behind it.
//
// Implementations must be safe for concurrent use and must not retain the
// Credential from a call.
type ProviderPort interface {
	Name() string
	// Endpoints reports which API surfaces this adapter speaks.
	//
	// It exists because the second real implementation needed it: an Anthropic
	// deployment serves /v1/messages and an OpenAI-compatible one serves
	// /v1/chat/completions, and routing a request to an adapter that cannot
	// speak its schema produces a confusing upstream 400 rather than a clear
	// gateway 404.
	//
	// This is a property of the adapter rather than of a deployment, because it
	// is determined by the wire protocol the adapter implements. A future
	// adapter that serves different surfaces per deployment would be the signal
	// to move it, and nothing here prevents that.
	Endpoints() []Endpoint
	Invoke(ctx context.Context, call *ProviderCall) (*ProviderResponse, error)
	Stream(ctx context.Context, call *ProviderCall) (ChunkStream, error)
}

// ---------------------------------------------------------------------------
// GuardrailPort
// ---------------------------------------------------------------------------

// Phase says which leg of the request a guardrail is inspecting.
type Phase uint8

// The two legs a guardrail can inspect.
const (
	PhaseRequest Phase = iota
	PhaseResponse
)

// Verdict is a guardrail's decision.
type Verdict uint8

// The decisions a guardrail can return.
const (
	VerdictAllow Verdict = iota
	VerdictDeny
	VerdictMutate
)

// FailureMode says what to do when a guardrail errors or exceeds its timeout.
//
// Fail-open guardrails are detection, not enforcement. Prompt-injection
// heuristics belong here: they are largely ineffective against a determined
// attacker, so shipping them as a blocking control buys an outage risk in
// exchange for security theatre. Secret scanning is fail-closed, because a
// leaked credential is not recoverable.
type FailureMode uint8

// The two failure modes a guardrail can be admitted under.
const (
	FailOpen FailureMode = iota
	FailClosed
)

// GuardrailInput is the payload and context handed to a guardrail.
type GuardrailInput struct {
	Phase   Phase
	Meta    RequestMeta
	Class   DataClass
	Tier    TrustTier
	Payload []byte
}

// GuardrailResult is a guardrail's decision. Payload is populated only for
// VerdictMutate and replaces the input for downstream stages.
type GuardrailResult struct {
	Verdict Verdict
	Reason  string
	Payload []byte
}

// GuardrailBudget is the latency and failure contract a guardrail is admitted
// under. It is declared in the component manifest and enforced by the caller,
// not trusted to the implementation.
type GuardrailBudget struct {
	Timeout  time.Duration
	Mode     FailureMode
	Blocking bool // non-blocking guardrails run off-path and can only alert
}

// GuardrailPort inspects, and optionally rewrites, a payload on either leg.
type GuardrailPort interface {
	Name() string
	Inspect(ctx context.Context, in *GuardrailInput) (*GuardrailResult, error)
}

// ---------------------------------------------------------------------------
// StorePort
// ---------------------------------------------------------------------------

// StorePort is split into sub-interfaces because its consumers need different
// slices of it, and an interface nobody implements in full is easier to fake in
// a test and harder to misuse in production.
//
// There is deliberately no SQL sub-interface here. The data plane never talks to
// a database: it holds no durable state and serves every request from its
// snapshot. Relational access is a control-plane concern and lives there.

// KVStore is ephemeral keyed state: rate-limit windows and the PII token vault.
// Losing it costs accuracy for one window, not correctness.
type KVStore interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error)
	Delete(ctx context.Context, key string) error
}

// EventStream is the append-only usage and audit stream. Publish is
// at-least-once; consumers are required to be idempotent.
type EventStream interface {
	Publish(ctx context.Context, events ...Event) error
}

// ---------------------------------------------------------------------------
// TelemetryPort
// ---------------------------------------------------------------------------

// TelemetryPort receives every event the gateway produces. Implementations fan
// out to OTel, Prometheus, a SIEM, or a tenant's own sink.
//
// Emit must not block the request path. An implementation that cannot keep up is
// required to shed and count, never to apply backpressure to a caller.
type TelemetryPort interface {
	Name() string
	Emit(ctx context.Context, events ...Event) error
}
