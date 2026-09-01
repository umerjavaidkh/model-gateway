# ADR 0005 — No identity closure table

**Status:** accepted · 2026-09-01
**Amends:** reference plan §5.1 ("Model as a materialized closure table, not
recursive CTEs at request time")

## Context

The plan is emphatic, and its reasoning is sound on its own terms:

> Model as a **materialized closure table**, not recursive CTEs at request time.
> A key resolves to a precomputed **principal record** in one hash lookup.

A closure table stores every ancestor–descendant pair so that "give me all
ancestors of this key" is one indexed read rather than a recursive walk. It is
the right answer when a deep or arbitrary-depth hierarchy has to be traversed on
a latency-critical path.

## Decision

Do not build one. Store the hierarchy as ordinary parent references and resolve
ancestry with a single join at snapshot build time.

## Why the justification does not apply here

**Nothing queries this at request time.** The data plane holds no durable state
and reads no database — that is the property the whole snapshot model exists to
provide. The clause the plan is defending against, "recursive CTEs at request
time", describes a system we deliberately do not have.

**The traversal happens once per build, not once per request.** The builder
already walks every tenant to compile a snapshot. Reading ancestry as part of
that walk costs one join over a table that is small by construction: the row
count is the number of API keys in the organisation, not the request rate.

**The hierarchy is fixed-depth.** `org → team → user | application → key` is
four levels, known when the schema is written. A closure table's value grows
with depth and with how unpredictable that depth is; at four known levels a
join names its own path and is easier to read.

**A closure table is a write-side liability.** It must be maintained on every
insert, move and delete, by triggers or by application code, and it is a
denormalisation that can silently disagree with the tree it summarises. Paying
that to speed up a query nobody makes on the hot path is the wrong trade.

## What would change this

Two things, either of which makes the closure table correct:

1. **Nested teams.** If a team can contain a team, depth stops being fixed and
   the join stops being expressible. The schema already allows for this —
   `teams.parent_team_id` exists and is nullable — so the change would be a
   migration plus a builder change, not a redesign.
2. **Snapshot build time becoming material.** If compiling a snapshot for the
   whole fleet takes long enough to delay configuration propagation past the
   30-second ceiling, the ancestry walk is a candidate to materialize. Measure
   before assuming it is the cause; the alias and budget joins are larger.

Until one of those is true, this is a table we would maintain, test and back up
in order to make an off-path query faster.

## Consequences

- The precomputed `Principal` the plan describes is unchanged. It is still
  flattened once, in the builder, and still resolves in one hash lookup on the
  data plane. That part of §5.1 is the part that matters, and it is kept.
- Ancestry correctness is enforced by foreign keys rather than by a
  denormalisation that has to be kept in step.
