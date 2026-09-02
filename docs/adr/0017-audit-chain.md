# ADR 0017 — The audit chain is written by one consumer, and does not record every request

**Status:** accepted · 2026-09-03
**Implements:** `model-gateway-architecture.md` §5.6

## Context

The plan says two things about audit, in one line each:

> **Audit records are separate from usage records** — append-only table with a
> hash chain, different retention clock.

and, in the PII section:

> **Audit tap sits after redaction, never before.**

Both are constraints rather than a design. Building it required deciding three
things the plan does not say: who computes the chain, what goes in it, and what
happens when the consumer needs to scale.

## Decision 1 — the chain is computed by the consumer, not the producer

`core.AuditEvent` was defined in M0 with `PrevHash` and `Hash` fields on it, as
if each worker would compute its own link. It cannot.

A hash chain is serial: to compute your link you need the hash of the record
before yours, and until it is written there is no "before yours". Two workers
emitting concurrently would each hash against the same predecessor and produce
a fork — two records claiming the same parent, which verifies as broken and
cannot be repaired without deciding which half of the history to throw away.

So the fields came off the event. The producer states a fact; the single
consumer that appends assigns the position and computes the hash. This is the
same reasoning that put cost computation in the *producer* (ADR: it is the only
place holding both the tokens and the price at that snapshot) applied in the
opposite direction: put the work where the information is, and here the
information is the end of the chain.

**What would make this wrong:** a deployment that needs audit ingestion to
scale horizontally. The answer then is one chain per shard with its own head —
partition by tenant, publish one head per partition — not a shared chain with
several writers.

## Decision 2 — not every request is audited

Auditing every successful call to an unclassified model would produce a second
copy of the usage stream with a stricter retention policy and no information
the first one lacks — at a volume that makes a hash chain unaffordable, since
appending is serial by decision 1.

What is recorded is **decisions**, where usage records **measurements**:

| recorded | why |
|---|---|
| every refusal | somebody may dispute it, and "when did we start refusing this" is unanswerable from aggregates |
| every redaction | "was this tenant's data sent to that provider" is the question the PII stage exists to answer |
| every access to data classified above public | who touched the restricted material cannot be reconstructed later |
| configuration changes | who granted this access, and when |

An ordinary successful call to a public model is in the usage stream, with its
stage timings, and the console links the two.

**What would make this wrong:** a compliance regime that requires an
append-only record of every access regardless of classification. That is a
policy flag on the tenant, not a redesign — the emission point already sees the
classification.

## Decision 3 — the chain detects tampering, it does not prevent it

Worth stating because it is easy to oversell. Anyone who can write the table
can rewrite the chain from the point they changed onward. What they cannot do
is change one row and leave the rest consistent.

So the property is *"a change is visible to anyone who checks"*, and it has two
holes that no amount of hashing closes:

- **A rewrite of the whole chain verifies.** Closing it means publishing the
  head somewhere the same person cannot write. `GET /v1/audit/verify` returns
  the head for exactly that; where it gets copied is deployment policy.
- **A record that was never written is invisible.** The chain covers the table,
  not the gap between the world and the table. The stream's at-least-once
  delivery and the appender's idempotency are what cover that.

Append-only is likewise a property of the code and of database grants, not of
the schema: Postgres has no insert-only table, and the migration does not
pretend otherwise.

## Consequences

- Audit ingestion is one process and does not scale out. A transaction-scoped
  advisory lock makes a second replica *safe* rather than useful — it takes
  turns — because "we only ever run one" is not something a deployment
  enforces.
- The sequence number is assigned by the appender, not by a database serial. A
  serial would let a second writer take a position without reading the head,
  which is the fork this exists to prevent.
- `Reason` carries values the gateway itself produced, never free-form error
  text. Upstream error messages can quote a request, and the audit tap sits
  after redaction precisely so this table does not become a copy of the data it
  protects.
- The hash covers the record's meaning and not its storage: not the row's
  insertion time, which the database chooses and which would make the chain
  unverifiable after a restore.
