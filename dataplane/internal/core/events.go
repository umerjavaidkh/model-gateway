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
}

// Kind identifies this as a usage record.
func (UsageEvent) Kind() EventKind { return EventKindUsage }

// OccurredAt reports when the request completed.
func (e UsageEvent) OccurredAt() time.Time { return e.Timestamp }

// AuditEvent records a security-relevant action. Consumers write these to an
// append-only table with a hash chain; PrevHash links each record to the one
// before it so that a deletion is detectable.
type AuditEvent struct {
	RequestID string
	Timestamp time.Time

	Tenant   TenantID
	Actor    string // key ID, user subject, or service-account name
	Action   string // "chat.completion", "key.rotate", "finetune.promote"
	Resource string
	Outcome  Code

	PrevHash string
	Hash     string
}

// Kind identifies this as an audit record.
func (AuditEvent) Kind() EventKind { return EventKindAudit }

// OccurredAt reports when the action took place.
func (e AuditEvent) OccurredAt() time.Time { return e.Timestamp }
