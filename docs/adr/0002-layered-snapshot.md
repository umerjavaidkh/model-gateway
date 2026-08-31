# ADR 0002 — The snapshot is layered, not monolithic

**Status:** accepted · 2026-08-31
**Amends:** reference plan §3 (control plane) and §4 (registry mechanics)

## Context

The plan's central idea is that the data plane serves every request from an
immutable, versioned snapshot and never reads a database. We keep that entirely.

The plan also says the snapshot is a single artifact containing the policy
bundle, routing table, model catalog, principal records and active plugin set —
and, separately, that plugins bind **per tenant, not globally**.

Those two statements do not compose. Per-tenant bindings, per-tenant aliases and
per-tenant budget state inside one artifact mean that one tenant editing a budget
rebuilds and reships every other tenant's configuration to every worker. At fifty
tenants this is invisible. At several thousand, with budget state updating from
the usage stream continuously, snapshot distribution becomes the system's
dominant cost and the "< 30 s propagation" ceiling stops holding.

## Decision

A snapshot is **composed from independently versioned layers**:

- **`GlobalLayer`** — model catalog, deployments, default aliases, default plugin
  bindings, policy bundle reference, and the key-prefix-to-tenant routing map.
  Large; changes rarely.
- **`TenantLayer`** — principals and key lookups, alias overrides, budget state,
  per-tenant plugin bindings, minimum trust tier. Small; changes constantly.

`Snapshot` is a pointer join over layers, never a merged copy. Replacing one
tenant layer costs a map copy of pointers and shares everything else, so a
tenant-level edit does not touch the catalog. Lookups resolve **tenant-first,
then global**, which is what makes "tenant A gets Presidio, tenant B gets
regex-only, tenant C gets none, same binary" a data question.

## Why this is in the first commit

The snapshot's identity is the contract between the control plane, the wire
format and every worker. Every consumer is written against its shape. Changing it
after routing, budgets and the registry are built is not a refactor — it is a
migration of all three plus a coordinated redeploy. The plan is right that some
things must be correct from day one; this is one of them, alongside the
`(baseModel, adapterId)` routing key.

## Consequences

- N−1 tolerance is now per layer, not per snapshot. A worker may hold global `N`
  with tenant `M` and tenant `M−1` simultaneously. The holder in M1 must track
  versions per layer and the usage event records both.
- Composition validates that every tenant layer has a key prefix routed to it. A
  snapshot that builds but authenticates nobody is far harder to diagnose than a
  build failure.
- Distribution can eventually ship only changed layers. That optimisation is not
  built yet, but the shape now permits it.
