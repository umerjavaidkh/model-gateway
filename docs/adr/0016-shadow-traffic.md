# ADR 0016 — Shadow traffic measures whether an adapter works, not whether it is better

**Status:** accepted · 2026-09-02
**Extends:** [ADR 0015](0015-weighted-rollout.md)
**Completes:** `model-gateway-architecture.md` §5.7

## Context

M11c left the canary walk driven by an operator, and said why: without a health
signal to advance on, a rollout that promoted itself would promote a bad
adapter just as reliably as a good one. This is that signal.

The plan describes it as: the adapter "takes shadow traffic (mirrored, response
discarded, scored offline), then walks the canary steps".

## The thing to be precise about

**"Scored offline" is doing a lot of work in that sentence**, and the honest
reading is narrower than it sounds.

The gateway can measure, for free, whether a mirrored request *errored*, how
long it took, and what it cost. It cannot measure whether the answer was
*better*. Doing that means either storing both payloads — which this system
deliberately does not do, and which would make the gateway a store of every
tenant's traffic — or running a judge over them, which is what the eval suite
already is.

So shadow traffic answers: **does this adapter fall over on real traffic?**
That is a real and common failure, and it is not the same question as "is this
adapter subtly worse", which the eval gate (ADR 0014) answered before any
traffic reached it.

Saying this plainly matters because "we shadow traffic and promote
automatically" is easily heard as "we compare answers", and a promotion
decision resting on that misunderstanding would be resting on nothing.

## Decision

**Mirroring must never touch the caller.** Not its latency, not its errors, not
its deadline. A shadow that can slow a request down makes production worse to
find out whether something might make it better, which is a trade nobody would
take if it were stated. So:

- `Send` is called after the response is on its way and after the usage event,
  and it neither blocks nor returns an error — that is the type signature, not
  a convention.
- Mirrors run on a context detached from the request's, which is cancelled the
  moment the response is written. Inheriting it would cancel every mirror
  before it started, and the feature would quietly do nothing.
- A fixed worker pool with a shallow queue, dropping rather than queueing.
  Mirrors are slower than the requests that spawn them, so a goroutine per
  request accumulates exactly when the system can least afford it, and a deep
  queue turns a slow shadow into stale data.

Each of those has a test that fails if it is removed.

**Sampled, not everything.** Mirroring doubles inference spend for whatever
fraction it covers. A shadow that costs as much as production is one an
operator turns off, and a feature that gets turned off protects nothing.
`shadow_percent` is a separate dimension from `weight`: an adapter in shadow
serves nobody while seeing real requests.

**A mirrored request gets its own usage event, marked `shadow`, priced at
zero.** Its own, because usage records are keyed by request id for idempotency
— reusing the id would make the accounting consumer discard one of the two as a
duplicate, and which one would depend on arrival order. Priced at zero because
nobody asked for it: charging a tenant would bill them for an experiment the
platform chose to run. The *cost* is still recorded, so the experiment's spend
is visible.

**The shadow runs the same transform as the primary.** Credentials resolved for
its own deployment, payload redacted for its own trust tier. A shadow measured
on a payload production never sends is measuring something else.

**Promotion compares against the base model, not against perfection.** A
provider having a bad hour must not abort every rollout riding on it. And the
tolerance is not zero: real traffic has a noise floor, and a gate that aborts on
noise is one an operator stops using.

**Three outcomes, not two: advance, abort, wait.** A canary at 1% of a quiet
tenant's traffic may never reach the evidence floor, and that is the correct
outcome — it waits, and an operator advances it by hand knowing they are doing
so without evidence. Collapsing "not yet" into "no" aborts healthy rollouts;
collapsing it into "yes" advances ones nothing has measured.

**Automatic advancement is off by default.** `GATEWAY_ROLLOUT_AUTOMATIC=true`
turns it on. A deployment that wants an operator to walk every step gets
exactly that, rather than an automation it has to remember to turn off — and
given what the signal can and cannot see, choosing not to promote on it is a
reasonable position rather than a timid one.

**Shadow and real traffic are counted together when judging.** An adapter's
errors are its errors whether or not anybody was waiting for the answer.
Separating them would mean a canary's first steps were judged on nothing while
its entire shadow window was ignored.

## Why not the alternatives

**Compare responses and score similarity.** The interesting version needs a
judge, and a judge is an eval suite — which already ran, on a fixed set,
before this started. The cheap version compares token counts and lengths, which
measures verbosity rather than quality and would promote on it.

**Store payloads for offline scoring.** Turns the gateway into a store of every
tenant's traffic. The design has refused this consistently — datasets stay
outside, PII is transformed on the way out — and one feature is not worth
reversing it.

**Mirror synchronously and discard.** Simpler, and it puts the shadow on the
request path. Everything above is a consequence of refusing that.

**Advance on a timer once the dwell elapses.** What M11c already rejected: it
promotes a broken adapter on schedule.

## Consequences

- `usage_records` gained `deployment` and `shadow`. Without deployment
  attribution "is this canary healthy" cannot be asked of the record at all —
  which is why it is indexed.
- A shadow that fails is a finding, not an incident: counted, logged, and never
  raised to a caller who got their answer from the base model.
- Shadowing stops once the adapter takes real traffic. There is nothing left
  for a mirror to say when the adapter is being measured on requests it
  actually answered.
- The migration's boolean default had to be `sa.false()` rather than the
  generated `sa.text('0')`: autogenerate ran against SQLite, and Postgres
  refuses an integer default on a boolean column. The Postgres migration test
  is what caught it, which is the reason that test runs against a real
  database.
