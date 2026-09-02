# ADR 0014 — The eval gate: integers, directions, and a baseline measured the same way

**Status:** accepted · 2026-09-02
**Extends:** [ADR 0013](0013-finetune-reconciler.md)
**Implements:** the eval gate of `model-gateway-architecture.md` §5.7

## Context

The plan's argument for the gate is the right one: a fine-tuned model's
regression is silent — no errors, just worse output — and the gate is what lets
an agent promote an adapter without a human reading the numbers, because the
criterion is machine-checkable rather than a judgement call.

The plan states it as:

```yaml
promotionGate:
  minScore: 0.87
  mustNotRegress: [latency_p95, refusal_rate]
```

Implementing that surfaced three things it leaves unsaid, and each of them is a
way for the gate to quietly mean nothing.

## Decision

**Scores are integer basis points, not fractions.** `0.87` against
`0.8699999999` is a comparison decided by the last bits of a double, and it can
go differently on two machines evaluating the same artifact. A gate whose
verdict is not reproducible is not a gate. Basis points — whole numbers out of
10,000 — compare exactly, and the API refuses a value outside that range so a
caller passing `0.87` or `87` fails loudly rather than passing everything.

**A metric carries its own direction.** `mustNotRegress: [latency_p95,
refusal_rate]` is meaningless without knowing that lower is better for both,
while higher is better for the headline score. Getting it backwards does not
fail loudly — it passes exactly the regressions the gate exists to catch. So
every metric declares `higher_is_better` or `lower_is_better`, the gate never
guesses, and comparing two metrics that disagree about their own direction is
an error rather than a verdict.

**A baseline is the base model measured by the same suite, in the same pass.**
"Must not regress" needs something to regress *against*, and the plan does not
say what. Storing a baseline per model would mean comparing against a number
measured by some earlier version of the suite; the gate refuses that
explicitly, because it is a comparison of two different measurements dressed up
as one. So the reconciler evaluates the candidate and the base model together,
and a scorecard is stamped with the suite and version that produced it.

Only when the gate needs it: a gate that is purely a minimum score is decided
from the candidate alone, and running a second evaluation for it would double
the cost to learn nothing.

**A missing baseline fails; it does not pass.** A gate that silently passes
when the thing it compares against is missing is a gate that stops working the
moment a baseline run fails — and stops working in the direction that promotes
things.

**The gate is recorded on the spec.** Fixed when the job is submitted, so
lowering the bar afterwards cannot retroactively promote something that already
failed it. Changing the gate for future jobs is a configuration change;
changing the verdict on a past one is not possible.

**A job with no suite stops at `TRAINED`, which is terminal.** An artifact
nobody has measured is one an operator promotes deliberately. The alternative —
promoting because no gate happened to be configured — makes the safe path the
one you get by forgetting something.

**Every reason, not the first.** A publisher looking at a rejected adapter
wants to know everything that has to change, rather than one thing per training
run, and a training run is hours.

**Scorecards are kept on failure.** That is exactly when someone wants to see
what was measured; discarding them means the only way to find out is to train
again.

## Who owns eval suites

The plan leaves this open, and suggests suites should be registered components
subject to the same signing and sandboxing as any other plugin. That is right:
a suite is code producing numbers a gate then trusts, so one that always
returns a perfect score promotes everything — it is exactly as
security-relevant as a guardrail.

This module resolves suites by name from a registry the deployment builds, the
same shape `Trainers` already uses, and does not yet bind them to the component
registry. That binding is one change covering both ports rather than one for
each, and doing it here would have meant doing it twice.

What is here now is the `EvalPort` contract suite, which is what a suite gets
checked against wherever it ends up living.

## Why not the alternatives

**Floats with a tolerance.** Picking the tolerance is picking a second, hidden
gate, and the argument about where it goes never ends.

**Declare directions in the gate rather than the scorecard.** The gate author
would have to restate what the suite already knows, and would eventually
restate it wrongly — for exactly the metric where being wrong is silent.

**Cache baselines per (suite, version, model).** A real optimisation, and it is
one: an eval run is minutes against a training run's hours, so it buys little
today and adds a cache whose staleness rules are the whole difficulty. Worth
revisiting when suites get expensive.

**Let the gate promote directly into the routing table.** `READY` means
eligible, not serving. A fine-tuned regression is silent, so entry is a
weighted rollout — shadow, then canary — which is the next module.

## Consequences

- `READY` is terminal here. It becomes non-terminal when rollout lands, which
  is a code change rather than a migration, because phases are a string column.
- The reconciler evaluates candidate and baseline in one pass rather than one
  per pass. An eval run is minutes; splitting it would mean carrying a
  half-finished comparison across a crash, where the risk is not cost but a
  verdict reached against a baseline from a different suite version.
- Scorecards are stored as JSON on the job row. They are written once, read
  once, and only ever compared against another scorecard — there is nothing to
  query across, and a metrics table would be a join for no reader.
- An eval backend outage leaves a job in `EVALUATING` and it recovers on the
  next pass. It must not fail an artifact that trained fine.
