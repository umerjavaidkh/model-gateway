# ADR 0011 — In-process components are WASM, with a fresh instance per call

**Status:** accepted · 2026-09-02
**Extends:** [ADR 0009](0009-component-registry.md), [ADR 0010](0010-sandboxed-admission-runner.md)

## Context

The registry has always described three execution modes. Two exist: `builtin`
for adapters compiled into the worker, and `sidecar` for a process on a Unix
socket. The third, `in_process`, is the one the design points at for
sub-millisecond work — routing strategies, deterministic detectors, cost
calculators — where a socket round trip costs more than the work itself.

A sidecar cannot serve that. Its isolation comes from the operating system and
it is paid for on every call.

## Decision

**wazero, not a CGo runtime.** The data plane builds as a static binary with no
C toolchain, and adopting wasmtime or WAMR would end that for one feature.
wazero is pure Go.

**The runtime is the sandbox — there is no container.** A WASM module has no
ambient authority at all: it cannot open a file, dial a socket, read an
environment variable, or address anything but its own linear memory. The only
things that reach it are the bytes the host copies in. That is a *stronger*
boundary than ADR 0010's container, and unlike that one it does not rest on a
shared kernel. So the admission runner skips the container for this mode —
isolation it does not need rather than isolation it forgot.

The runtime is given WASI only because a Go-compiled guest's runtime needs
clocks and entropy to start. No filesystem, no environment, no arguments, no
standard streams: a guest asking for a file gets the same answer as one asking
for a network, which is that there is nothing there. `TestTheGuestCannot…`
asserts this rather than assuming it.

**Two exported functions and no host functions.** `alloc(size) -> ptr` and
`handle(ptr, len) -> ptr<<32|len`. A richer interface means host functions, and
every host function is a capability granted to code nobody has vetted. The
reason this boundary is worth having is that there are none.

**The same JSON as the sidecar protocol.** A publisher moving a component
between execution modes changes how it is built, not what it says. Two
encodings for one contract would drift, and the drift would surface as a
component that behaves differently depending on how it was deployed.

**A fresh instance per call.** This is the load-bearing decision. Without it a
component could stash one tenant's payload and return it to another, and
nothing outside the module could see that happen. It is what makes running
someone else's code in the worker's own process defensible at all.

It costs **1.6ms** for a guest compiled from Go (`BenchmarkCall`), and roughly
60% of that is wazero reserving the module's linear memory rather than the
guest's own start-up — measured by instantiating with and without
`_initialize`. A component that needs to be faster needs a leaner guest, not a
weaker boundary; a component that cannot meet its declared latency budget fails
at admission rather than in production, which is what the budget is for.

That number is in a benchmark in the repository, so a future argument for
pooling instances has to be made against a measurement rather than an
intuition.

**Modules are addressed by the digest of their own bytes.** There is no
registry to pin against — the artifact is a file — so the manifest carries
`sha256:…` of the module and both the runner and the worker verify the bytes
before compiling. How the file arrives is a deployment concern an organisation
already has an answer to: an init container, a volume, a sync sidecar. What is
not solved, and is ours, is making sure the bytes that arrive are the bytes
that were admitted.

**How a component runs travels in the snapshot.** `GuardrailBinding` gained
`execution` and `module`, because the snapshot is the worker's only source of
configuration. A worker that had to ask the control plane how to run something
it had already been told to run would stop serving when the control plane was
down — which is exactly the property the layered snapshot exists to protect.

**Modules are compiled on snapshot apply, not on lookup.** Compilation is
hundreds of milliseconds. Doing it on first use would put that on one arbitrary
tenant's request; `Holder.OnApply` runs the loader before the snapshot becomes
visible, so no request can arrive to find a binding whose component is not
loaded yet. The cache is keyed by digest, so a component that changes version
without changing its module is not recompiled.

**Built-ins win a name collision.** The composite registry consults the static
registry first, so a published component sharing a name with one of ours cannot
displace it. The alternative is a registry entry silently overriding a
guardrail an operator believes is running.

## Why not the alternatives

**Reuse one instance across calls, like a sidecar.** Microseconds instead of
milliseconds, and the argument that a sidecar has the same property is
genuinely true. It is rejected because the host *can* enforce isolation here
and a sidecar cannot: declining a boundary that is available and cheap to state
is different from accepting one that is unavailable.

**A pool of instances.** Same state-leakage risk as reuse, with more machinery.
Worth revisiting only with a profile showing instantiation is the bottleneck in
a real deployment.

**Make per-call isolation configurable.** A security knob whose safe setting is
the slow one gets turned off, and the person who turns it off is not the person
whose data leaks.

**Let the worker fetch modules itself.** A network dependency in the worker's
load path, for a problem — distributing files — that every deployment already
solves.

## Consequences

- A guest compiled from Go is ~3 MB and takes ~400ms to compile. That is once
  per module per worker, at load. A TinyGo or Rust guest is far smaller.
- Only the guardrail port has a WASM adapter so far. Extending it is a new
  adapter over the same host, not new isolation machinery.
- A component whose artifact has not landed is left unbound and logged, rather
  than failing the whole snapshot: refusing a configuration because one file is
  late would make every rollout wait on file distribution.
- The worker's `GATEWAY_WASM_DIR` is optional. A deployment binding no
  in-process components starts no WASM runtime at all.
