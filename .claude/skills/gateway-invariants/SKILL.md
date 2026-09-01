---
name: gateway-invariants
description: The Model Gateway's non-negotiable design rules, checked before writing or reviewing code in this repository. Use when implementing a module, reviewing a diff, or deciding whether a change belongs where it is being put. Covers the layering rules (core imports nothing, no database in the data plane, wire vs domain vs rows), the request-path contracts (one lease per request, trust tier pinned before candidates, one usage event on every path), the money and secrets rules, and when a deviation from model-gateway-architecture.md needs an ADR.
---

# Model Gateway invariants

Rules that hold everywhere in this repository. Most are enforced by CI; the ones
that are not are the ones worth re-reading, because nothing will catch them.

## Layering

- **`dataplane/internal/core` imports nothing but the standard library.** CI
  enforces it (`scripts/check-core-imports.sh`). If a change to `core` needs an
  import, the change belongs somewhere else.
- **The data plane never touches a database.** It holds no durable state and
  serves every request from its in-memory snapshot. `StorePort` has no SQL
  sub-interface so this is structural, not conventional.
- **Three shapes stay separate: wire, domain, rows.** Wire types are generated,
  append-only, shared between languages. Domain types are refactored freely.
  Database rows are normalised for storage. Exactly one module translates each
  pair. Collapsing any two makes every rename a compatibility question.
- **Dependencies are constructor arguments.** No module-level singletons. The
  tell that this was violated: a test that patches a module global.

## The request path

- **One lease per request.** `lease := holder.Acquire(); defer lease.Release()`
  immediately. A leaked lease pins a snapshot generation and its plugins for the
  life of the process.
- **Snapshot-resident values are immutable.** Accessors return snapshot-owned
  memory: read it, never write to it.
- **The trust tier is pinned before any candidate is considered.** Never filter
  per-candidate; a fallback that changes tier sends a payload redacted for the
  wrong destination.
- **Exactly one usage event per request, on every exit path** — including 401s,
  budget refusals and mid-stream failures. A request that burned upstream tokens
  before erroring still costs money.
- **Nothing blocks unbudgeted.** Guardrails declare a timeout and failure mode;
  the caller enforces it rather than trusting the implementation.

## Contracts that bite

- **Money is integer micro-USD.** Never float. Truncate down, never up.
- **Secrets never enter a snapshot.** Deployments carry a `CredentialRef`;
  workers resolve it lazily with a TTL, and a failed fetch is not cached.
- **Tenant ID is never a metric label.** Unbounded cardinality, and Prometheus
  keeps series after they stop receiving samples. Use tenant *tier*.
- **`io.EOF` arrives alone from a `ChunkStream`.** Returning a chunk with it is
  the Go iterator trap: callers `break` and silently drop the token usage.
- **Every `core.Code` needs a row in `statusByCode`.** An unmapped code returns
  500 by design, because an unmapped code is a bug in whoever added it.
- **A new `ProviderPort` implementation must pass `internal/contracts`.** That
  suite is also the registry's admission gate.

## Process

- **A departure from `model-gateway-architecture.md` needs an ADR** in
  `docs/adr/`, naming what would make the decision wrong. Deviating is expected;
  doing it silently is not.
- **Concurrency changes need a `-race` test.** A performance claim needs a
  benchmark in the repo.
- **Coverage is gated at 80%.** Raise it when the code earns it; never lower it
  to make a red build green.
- **Verify with `make check` and check its exit code.** Piping it into `grep`
  masks the exit status and a failing check looks clean.

## Review order

Cheapest signal first:

1. Correctness under concurrency — shared mutable state, blocking calls in a
   hot path, a lease that can leak on an error return.
2. Resource lifetime — streams and connections without a deterministic close.
3. Layering — did this put a decision in the transport, or a vendor in `core`?
4. Interface shape — a bare map where a typed record belongs.
5. Performance, only with a measurement in hand.
