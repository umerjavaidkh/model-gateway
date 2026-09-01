# ADR 0006 — Compile policy to a decision table; do not adopt a policy language yet

**Status:** accepted · 2026-09-02
**Settles:** reference plan §8 ("Cedar vs OPA"), implements §5.3

## Context

The plan says two things about policy, and they pull in different directions:

> **Adopt** — Cedar (or OPA). Formally verified, fast, well understood. Writing
> a policy language is a career.

> **Compile** policies to a decision function in the control plane; never
> interpret rules per request.

The second is the architectural claim, and it is the one that matters here. A
gateway that parses and interprets policy on every request has put a language
runtime in its hot path, which is exactly what the snapshot model exists to
avoid.

## Decision

Separate the two questions, which the plan's framing conflates:

1. **What policy is written in** — a source language, authored by humans.
2. **What the data plane evaluates** — a compiled artifact carried in the
   snapshot.

Build (2) now, as a flat decision table of ordered rules over a fixed attribute
set. Defer (1): the current authoring format is a small declarative rule list,
not a language, and it is deliberately not extensible into one.

## Why this is not "writing a policy language"

The plan's warning is about semantics, not syntax. What makes a policy language
a career is the part that follows the parser: evaluation-order guarantees,
totality, conflict resolution, formal verification, and the decade of edge
cases that follow. None of that is here. What is here is an ordered list of
conditions over six attributes, evaluated first-match-wins, with no negation,
no recursion, no joins and no data documents.

That is a decision table. It is deliberately less expressive than Cedar or
Rego, and being less expressive is the feature: every rule can be understood in
isolation, and the whole bundle can be read top to bottom.

## Why Cedar or OPA is not adopted today

**They solve the authoring problem, which we do not have yet.** There is one
rule shape and no tenant-authored policy. Adopting a language now means
carrying its runtime, its version skew and its failure modes to express what a
list currently expresses.

**Both would still need compiling.** Interpreting Cedar per request is the
thing §5.3 forbids, so adopting it properly means compiling Cedar *into*
something like this table anyway. Building the table first is building the part
that survives either choice.

**The port is the compiled form, not the syntax.** When tenant-authored policy
arrives, Cedar becomes a front end that emits this bundle. Nothing in the data
plane changes, because the data plane never sees the source.

## What would change this

- **Tenants authoring their own policy.** A decision table is fine for a
  platform team and wrong as a customer-facing surface; that is when a
  language's formal properties start paying for themselves.
- **A rule that cannot be expressed as a first-match condition list** —
  anything needing negation over sets, or a decision that depends on the
  outcome of another rule.

Either is a signal to adopt Cedar as the front end. Neither is a reason to
interpret it per request.

## Consequences

- An empty bundle allows. Policy is one control among several — the model
  allowlist, budgets and trust tiers already restrict — and a bundle that
  denied by default would make adding the feature an outage.
- The bundle is snapshot content, so a policy change propagates exactly like
  every other configuration change and rolls back the same way.
- Evaluation is an array scan over a handful of integers and string
  comparisons, with no allocation on the allow path.
