package core

import "time"

// EventKind discriminates the closed set of events the gateway emits.
type EventKind string

// The kinds of event the gateway emits.
const (
	EventKindUsage EventKind = "usage"
	EventKindAudit EventKind = "audit"
)

// Event is a sealed interface: only the types in this file implement it.
//
// Usage and audit are deliberately separate types rather than one event with a
// grab-bag of optional fields. They have different consumers, different
// retention clocks, and different integrity requirements — audit records are
// hash-chained and append-only, usage records are aggregated and expired.
// Collapsing them into one shape loses all of that distinction at the type level
// and it stops being enforceable.
type Event interface {
	Kind() EventKind
	OccurredAt() time.Time
}

// UsageEvent is emitted once per request. The stream of these is the source of
// truth for cost accounting, so it is emitted even for failed requests: a
// request that burned upstream tokens before erroring still cost money.
type UsageEvent struct {
	RequestID string
	Timestamp time.Time

	Tenant TenantID
	KeyID  KeyID
	// Tier is the tenant's plan tier, carried because it is safe to use as a
	// metrics label. Tenant ID is not: a per-tenant Prometheus label is
	// unbounded cardinality and will take Prometheus down. Tenant ID belongs in
	// this event and in exemplars, never in a label.
	Tier string

	Deployment DeploymentID
	Route      RoutingKey
	// Provider is the adapter that served the call. It is a bounded set, so
	// unlike the tenant id it is safe as a metrics label.
	Provider string
	Stream   bool
	// Shadow marks a mirrored request: one nobody was waiting for, whose
	// answer was discarded. It cost real money and belongs in the record, but
	// it is not traffic anybody was served.
	Shadow bool

	// Stages is how long each leg of the request took, in the order they ran.
	//
	// Recorded on the event rather than left to tracing because the two answer
	// different questions. A trace answers "what happened to this one request"
	// for someone who already knows which request to look at; this answers
	// "which stage is slow" and "what did that refusal actually cost" for
	// someone looking at a thousand of them — and it survives without a trace
	// backend, which most deployments will not have on day one.
	Stages []StageTiming

	InputTokens  int64
	OutputTokens int64
	// CachedInputTokens and CacheWriteTokens are recorded separately because
	// they are billed at different rates, and because a request's cache hit
	// rate is unrecoverable after the fact — the provider reports it once.
	CachedInputTokens int64
	CacheWriteTokens  int64

	// CostMicroUSD is what the provider charges us. PriceMicroUSD is what the
	// tenant is charged. They are equal today because no rate card exists yet,
	// and they are separate fields because the moment one tenant has a
	// negotiated rate, a markup or a committed-use tier, conflating them is
	// wrong in the direction of a customer dispute — and budgets are enforced
	// against price, not cost.
	CostMicroUSD  MicroUSD
	PriceMicroUSD MicroUSD

	LatencyMs       int64
	TimeToFirstByte time.Duration
	Outcome         Code

	// SnapshotVersion records which configuration served the request. Without
	// it, "why did this request route there?" is unanswerable after a rollout.
	SnapshotVersion uint64

	// Budgets are the budgets this request must be charged against, captured
	// when it was served rather than looked up by the consumer afterwards. A
	// budget detached from a key later would otherwise lose the spend incurred
	// while it was attached, and the arithmetic would quietly stop matching
	// the invoice.
	Budgets []BudgetID
}

// Kind identifies this as a usage record.
func (UsageEvent) Kind() EventKind { return EventKindUsage }

// OccurredAt reports when the request completed.
func (e UsageEvent) OccurredAt() time.Time { return e.Timestamp }

// StageTiming is one leg of the request path and what it cost.
type StageTiming struct {
	// Name is the stage: authenticate, admit, guard, route, adapt.
	Name string
	// Duration is wall time inside that stage.
	Duration time.Duration
	// Outcome is empty when the stage passed, and otherwise the code it
	// refused with. A stage that refused is where the request ended, so this
	// is what makes a failure readable without opening a trace.
	Outcome Code
}

// AuditEvent records a decision, where a UsageEvent records a measurement.
//
// That is the line between them: a refusal, a redaction and a configuration
// change are things somebody decided, and they are kept append-only with a hash
// chain and their own retention clock. Token counts are things that happened,
// and they are aggregated and expired.
//
// The hash chain is deliberately not on this type. Two workers emitting
// concurrently would each need the previous record's hash to compute their own,
// and there is no previous record until one of them is written — so the chain
// is computed by the single consumer that appends, not by the producers. A
// producer that computed its own would fork the chain the moment a second
// replica started, and a forked chain proves nothing.
type AuditEvent struct {
	// EventID makes the record idempotent under at-least-once delivery. The
	// request id cannot: one request can produce both a refusal and a
	// redaction, and a configuration change has no request at all.
	EventID string
	// RequestID is empty for actions that were not a request.
	RequestID string
	Timestamp time.Time

	Tenant TenantID
	// Actor is who did it: a key ID, a user subject, or a service-account name.
	Actor string
	// Action is what they did: "chat.completions", "key.issue".
	Action string
	// Resource is what they did it to: a model, a deployment, a component.
	Resource string
	// Outcome is empty when the action was allowed, and otherwise the code it
	// was refused with.
	Outcome Code
	// Reason is the human-readable half of the outcome: which guardrail
	// refused, which rule matched. Never the payload that triggered it — the
	// audit tap sits after redaction precisely so this record does not become
	// a copy of the data it exists to protect.
	Reason string

	// SourceIP is the caller's address, for the "who, and from where" an
	// investigation starts from.
	SourceIP string
	// SnapshotVersion is the configuration in force when the decision was
	// made, which is what makes "why was this allowed in March" answerable.
	SnapshotVersion uint64
}

// Kind identifies this as an audit record.
func (AuditEvent) Kind() EventKind { return EventKindAudit }

// OccurredAt reports when the action took place.
func (e AuditEvent) OccurredAt() time.Time { return e.Timestamp }
