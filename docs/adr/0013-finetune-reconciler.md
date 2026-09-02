# ADR 0013 — Fine-tune jobs are reconciled, and the idempotency key is the safety property

**Status:** accepted · 2026-09-02
**Implements:** `model-gateway-architecture.md` §5.7, in part

## Context

The plan describes fine-tuning as a declarative `spec` + `status` reconciled by
a control-plane loop, because an agent drives it: an agent POSTs a spec and
polls, rather than orchestrating upload → submit → poll → commit and trying to
recover when step four fails.

Everything else in this system is cheap to get wrong twice. A duplicate usage
event is discarded; a duplicate snapshot build produces the same bytes; a
duplicate contract-suite run wastes a container. A duplicate *training run*
books hours on eight GPUs and nobody notices until the invoice.

That difference is what this module is designed around.

## Decision

**A reconciler, not a workflow engine.** Each pass takes one job, does the
single next thing, and writes down what happened. A job needing five steps
takes five passes. A pass that crashes leaves the job somewhere a later pass
resumes from, which is the property that matters — the alternative is a
half-finished orchestration nobody can resume.

**Submission is written down before it is attempted.** `PENDING → SUBMITTING`
is committed *before* the trainer is called. A job found in SUBMITTING on
restart is one whose fate is unknown: it may have reached the trainer, it may
not. The reconciler resolves that by calling submit again with the same
idempotency key rather than guessing, and it can only do that because the row
exists.

**The `TrainerPort` contract requires `submit` to be idempotent on that key.**
This is the load-bearing requirement of the whole module. An adapter for a
backend with no idempotency of its own has to build it — tag the run with the
key, search before creating — and doing that badly still beats not doing it,
because the failure mode of not doing it is a duplicate bill.

There is a contract suite (`contracts/trainer.py`) that any adapter can run,
and its idempotency case is the reason it exists. A suite that only checked
type signatures would pass an adapter that starts a second run every time.

**The key is generated server-side and is globally unique.** Never accepted
from a caller. Two jobs sharing a key would each get the same run back from a
correctly idempotent trainer — the second silently adopting the first's
training and its artifact, with both looking successful. A unique constraint
enforces it rather than a convention.

**Terminal states are enforced in the domain.** A reconciler that re-submits a
finished job books a second run, and "the loop has a bug" is not a good reason
for that to be possible. `TRAINED`, `FAILED` and `CANCELLED` refuse every
transition.

**The reconciler owns its transactions; the service does not.** Every other
service here takes a request-scoped session and leaves committing to its
caller. This one cannot: the whole point of writing SUBMITTING before calling a
trainer is that the write is durable when the call happens, and a service that
defers committing cannot promise that. So they are two classes with one rule
each, rather than one class with a rule that depends on which method you called.

**A row lock is not what makes concurrency safe here, and the code says so.**
Committing SUBMITTING releases any lock the transaction held, so no lock can
span the external call that follows it. A second reconciler genuinely can call
submit for the same job; the idempotency key is what makes that cost nothing.
The lock in `_advance` still earns its place — it stops two passes interleaving
writes to one job's status, and `skip_locked` lets a replica move on rather
than queue — but claiming it as the safety property would be false.

This was written the other way round first. The concurrency test is what
showed that the lock could not do the job, and the fix was to correct the claim
rather than to add a lease for something the key already handles.

**Datasets stay outside the gateway.** A `DatasetRef` is a pointer: URI,
checksum, rows, schema version. The checksum is required, because without one
the data behind that URI can change after the job runs and "which data produced
this adapter" has no answer.

**Training spend is its own budget dimension.** `budget_ref` is separate from
the inference budget, as the plan requires. One fine-tune can exceed a month of
serving, and a shared budget means training silently starves inference or the
reverse. Cost is recorded on the job as it accrues, so a run that fails halfway
still accounts for what it burned.

## What this module does not do

Deliberately, and each is a separate PR:

- **No eval gate.** A job ends at `TRAINED`. Whether an artifact may serve
  traffic is the eval gate's decision — the same shape as M10a's binding gate,
  which refused everything until M10b could vouch for it. `eval_suite` is
  recorded on the spec so the gate a job will face is fixed when it is
  submitted, not whenever it finishes.
- **No rollout.** Shadow, canary and weighted promotion need the artifact in
  the routing table, which needs the gate first.
- **No trainer adapter.** The port, the contract suite and the reconciler are
  the deliverable. Shipping an adapter for a backend that cannot be reached
  from this repository would mean shipping code nothing has ever run — the
  contract suite is worth more, because it is what an adapter is checked
  against wherever it is written.

## Why not the alternatives

**A workflow engine (Temporal, Airflow).** A real answer for this problem, and
a large operational dependency for one loop over one table. The reconciler is
about two hundred lines and the durability argument above is the whole of its
complexity.

**Poll the trainer from the API process.** Training runs take hours. A request
path that waits on one is not a request path, and a background thread inside
the API process makes the API's restart a training outage.

**Let the client supply the idempotency key.** It reads as more RESTful and it
is actively dangerous: a client reusing a key across jobs makes two jobs share
one training run.

**Store spec and status in separate tables.** They are written together, read
together and locked together. Splitting them buys a join and a way for the two
halves to disagree about which generation of a job is being reconciled.

## Consequences

- A deployment with no trainer configured accepts jobs that never advance. The
  API refuses a job naming a trainer it does not have, so the mistake surfaces
  at submission; the reconciler process logs a warning at startup.
- `SELECT ... FOR UPDATE SKIP LOCKED` is a no-op on SQLite, so the concurrency
  test is skipped there and gated on Postgres — the same discipline the KV
  contract suite already uses for Redis.
- Phases are a string column rather than a database enum, so adding one (the
  eval phases, next) is a code change rather than a migration on a table that
  may be large.
