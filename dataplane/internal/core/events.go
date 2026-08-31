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

	InputTokens  int64
	OutputTokens int64
	CostMicroUSD MicroUSD

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
