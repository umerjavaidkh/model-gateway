package gateway

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// The audit trail records decisions, where the usage stream records
// measurements. Both are emitted from the same exit path, because a decision
// that is not recorded on the path that refused is a decision that goes
// missing exactly when somebody asks about it.
//
// Not every request produces one. Auditing every successful call to an
// unclassified model would make this a second copy of the usage stream with a
// stricter retention policy and no extra information — and the volume is what
// makes a hash chain unaffordable. What is recorded:
//
//   - every refusal, because a refusal is a decision somebody may dispute;
//   - every redaction, because "was this tenant's data sent to that provider"
//     is the question the PII stage exists to answer;
//   - every access to data classified above public, because "who touched the
//     restricted material" cannot be reconstructed later from aggregates.
//
// What is deliberately absent is the payload. The audit tap sits after
// redaction, and Reason carries structured facts the gateway produced — never
// free-form error text, which can quote an upstream response and would turn
// this table into the thing it exists to protect.

// auditable reports whether a request produced a decision worth chaining, and
// why. An empty action means it did not.
func auditable(req *Request, principal core.Principal, redactions int, err error) (string, string) {
	switch {
	case err != nil:
		return string(req.Meta.Endpoint), ""
	case redactions > 0:
		return "pii.redact", fmt.Sprintf("redacted %d values before sending", redactions)
	case principal.DefaultClass != "" && principal.DefaultClass != core.DataClassPublic:
		return string(req.Meta.Endpoint), "data classified " + string(principal.DefaultClass)
	default:
		return "", ""
	}
}

// emitAudit records one decision, if this request was one.
//
// The event id is the request id joined to the action rather than a fresh
// random value, so a redelivered event is recognisably the same record. One
// request can produce at most one audit event today; the action is in the key
// so that stays true if it ever produces two.
func (p *Pipeline) emitAudit(
	ctx context.Context, snap *core.Snapshot, req *Request, result *Result,
	redactions int, err error,
) {
	action, reason := auditable(req, result.Principal, redactions, err)
	if action == "" {
		return
	}

	// The model the caller asked for, not the deployment that served it: an
	// audit answers what was requested, and a refusal has no deployment at all.
	resource := req.Meta.Model
	if result.Deployment != "" {
		resource = string(result.Deployment)
	}

	_ = p.telemetry.Emit(ctx, core.AuditEvent{
		EventID:         req.Meta.RequestID + ":" + action,
		RequestID:       req.Meta.RequestID,
		Timestamp:       p.now(),
		Tenant:          result.Principal.Tenant,
		Actor:           string(result.Principal.KeyID),
		Action:          action,
		Resource:        resource,
		Outcome:         core.CodeOf(err),
		Reason:          reason,
		SourceIP:        sourceIP(req.Meta.SourceIP),
		SnapshotVersion: snap.GlobalVersion().Number,
	})
}

// sourceIP renders the caller's address, empty when there was none to read.
//
// The zero Addr formats as "invalid IP", which would go into the record as if
// it were an address somebody could investigate.
func sourceIP(addr netip.Addr) string {
	if !addr.IsValid() {
		return ""
	}
	return addr.String()
}
