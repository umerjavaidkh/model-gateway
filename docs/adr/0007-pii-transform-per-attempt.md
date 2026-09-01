# ADR 0007 — Transform per attempt, not per request

**Status:** accepted · 2026-09-02
**Revises:** the agreed deviation recorded in
[`plan-deviations`](0002-layered-snapshot.md) as ② — "pin the trust tier at
selection and filter the whole candidate list to it"

## Context

The reference design places the PII transform *after* routing, because whether
to redact depends on where the request is going: a vLLM pod inside the VPC
needs no redaction, an external provider does.

Before this module there was no transform, and the risk was purely that a
fallback could move a request to a lower trust tier than its policy allowed. So
M2 pinned a minimum tier at selection and filtered the whole candidate list to
it. That was correct then, and a test asserts it still holds.

But "every candidate meets the minimum" is not "every candidate has the same
tier". A list filtered to *at least external* legitimately contains both an
internal deployment and an external one. With a transform in play, a payload
prepared for the first is wrong for the second — and the direction of that
error is a payload redacted less than its destination requires.

## Decision

Transform inside the execution attempt, parameterised by the deployment that
attempt is actually calling.

The router's execution closure already receives the deployment. Each attempt
therefore builds its own payload from the original, at the strategy its own
destination requires.

## Why not the alternatives

**Pin the list to a single tier.** Would work, and would throw away the
fallback that makes the router useful: a tenant with one internal and two
external deployments would lose the externals entirely rather than fall back to
them with heavier redaction.

**Transform once at the lowest tier present.** Every request would be redacted
as though it were going to the least trusted destination, including the ones
that go internally. That is safe and wasteful, and the waste is accuracy —
redaction costs the model context it could have used.

**Transform once after selection, before execution.** Correct only while the
first candidate is the one that serves. It fails exactly when the router does
its job, which is the worst time for a security control to be wrong.

## Consequences

- The original payload is retained for the duration of execution, because each
  attempt derives from it. It is never sent.
- A retry across tiers re-tokenises, so placeholder numbering restarts per
  attempt. That is invisible to the caller, whose response is restored against
  the replacements of the attempt that actually served it.
- The tier filter from ② stays. It prevents a different failure — routing to a
  destination policy forbids entirely — and this ADR does not weaken it.
