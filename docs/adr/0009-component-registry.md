# ADR 0009 — The registry gates bindings; the gate never runs in the control plane

**Status:** accepted · 2026-09-02

## Context

The design's core constraint is that every component is replaceable through a
registry, bound per tenant, activated by the next snapshot rather than by a
deploy. Until now the binding half existed and the registry half did not: a
snapshot could name `presidio@2.1.0` whether or not such a thing had ever been
registered, tested, or written.

That is not a small gap. A binding that names nothing resolves to nothing, and
a guardrail that resolves to nothing is a control an operator believes is
enforcing while it is not. The data plane handles it as well as it can — it
warns, and refuses the request if the binding is fail-closed — but by then the
mistake has already reached every worker.

The plan also asks for the other half: third parties registering components
that the gateway then runs. That is a code-execution surface, and the phrase in
the design is exact — without a gate it is "a remote-code-execution
vulnerability with a nice admin UI."

## Decision

**A component is a manifest plus an admission record, and only the record makes
it bindable.** Registration stores what a publisher claims. Admission stores
what a contract suite observed. They are separate calls with separate
authority, because collapsing them makes "register" the operation that grants
production access.

**The snapshot builder refuses any binding the registry cannot vouch for.**
Unregistered, pending, retired, wrong port, or a guardrail given less time than
its manifest declares it needs — each fails the build. This is the registry's
only teeth: without it the tables are a record nobody consults.

**The admission record binds to a manifest digest, not to a name and version.**
Editing an admitted manifest loses its admission rather than inheriting it. The
invariant is enforced in the constructor, so a component that is active without
a passing run covering its current bytes cannot be built at all.

**The latest run is authoritative.** A failing re-run demotes an active
component to pending. That is not an outage: snapshots already built keep
serving and workers keep running what they have — only the *next* build refuses
to bind it. A flaky suite therefore blocks the next configuration change
visibly, rather than leaving something bindable that nothing vouches for.

**Retirement is not deletion.** The row stays, existing snapshots stay valid,
and an operator binding a withdrawn component is told it is retired rather than
that it does not exist — which would send them looking for a typo.

**The control plane does not run contract suites.** `AdmissionGate` is a port,
and its default implementation refuses everything. Running a suite against a
third-party component means executing code we did not write, in a process
holding database credentials, the key pepper, and the network position of the
thing that configures every worker. A deployment that wants an open registry
configures an ephemeral, resource-limited, offline sandbox; the absence of one
fails closed.

**Versions are semver and images are pinned by digest.** A version that does
not identify an artifact — `latest`, `2.1` — cannot be what an admission
covers. A floating tag turns the admitted artifact into a different one
silently, which defeats the whole gate.

## Why not the alternatives

**Validate bindings in the data plane instead.** It already does what it can,
and it is the wrong place to *decide*: a worker cannot tell "this component was
never admitted" from "this worker is one release behind". The first is a
configuration error that should never have compiled; the second is a normal
rolling deploy.

**Let the control plane run the suite in-process, sandboxed by convention.**
There is no such sandbox. A Python process with an event loop and a database
handle is not an isolation boundary, and calling it one is worse than admitting
there is none.

**Admit on registration and audit afterwards.** Then the registry is a list of
claims, and every binding downstream inherits that. Afterwards is after the
component has served traffic.

**Trust a signature instead of a suite.** A signature says who published
something, not that it works. Both are worth having and they answer different
questions; signing is deferred to its own change rather than conflated with
this one.

## Consequences

- An empty registry means no bindings compile. There is no permissive mode:
  "the registry is empty so allow everything" is the failure this removes.
- Components compiled into the worker are registered like any other, with an
  admission whose runner is this repository's CI. A declarative registry in a
  config file may state an admission — that path is an operator compiling from
  a file they already control, and it is the only way builtins get registered
  at all. The gate governs the API, where the submitter is not the operator.
- Nothing is admitted until a sandboxed runner exists. Until then the registry
  is a strict allowlist maintained by operators, which is a useful thing to be.
- `Port` gained the two control-plane ports. A binding for one is a build
  error, because a trainer never reaches a worker and encoding it would ship a
  field the data plane silently ignores.
