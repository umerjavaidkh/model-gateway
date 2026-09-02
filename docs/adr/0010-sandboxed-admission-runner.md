# ADR 0010 — The admission runner is a separate deployable that can only report

**Status:** accepted · 2026-09-02
**Extends:** [ADR 0009](0009-component-registry.md)

## Context

ADR 0009 left one thing undone on purpose: `AdmissionGate` was a port whose
default refused everything, so nothing could actually be admitted. The registry
was a strict allowlist maintained by hand.

Filling that port means running a contract suite against a component the
gateway did not write. There is no version of that which does not execute
untrusted code, and the design says so plainly: without a real boundary a
plugin registry is "a remote-code-execution vulnerability with a nice admin
UI."

## Decision

**A third binary.** The gateway must never execute an unadmitted component; the
control plane must never execute one at all — it holds database credentials,
the key pepper, and the network position of the thing that configures every
worker. `admissionrunner` runs on a disposable host, does the one dangerous
thing, and exits.

**The runner reports; it does not decide.** It produces a record naming the
suite, the manifest digest it examined, and the result. The control plane
checks that digest against what it has registered and decides what the verdict
means. A runner that could activate a component would be a runner whose
compromise is an activation.

**One battery, two callers.** The contract suites now take a `contracts.T`
rather than a `*testing.T`, so `go test` and the runner execute the same code.
A second battery maintained for the runner would drift, and the drift would
always be in the direction of the runner's copy asking for less. `Recorder` is
the non-test implementation: it isolates a `Fatal` to its own case in a
goroutine, exactly as `testing` does, so one failed assertion produces a report
with eleven other results rather than a report with one.

**An empty report has not passed.** A battery that ran nothing proved nothing.
Treating "no failures" as success would admit a component the runner never
reached.

**"Could not test" is not "failed".** The runner exits 1 when the run could not
happen and 2 when it happened and the component failed. Conflating them lets an
infrastructure problem look like a component defect — and makes a CI job retry
a genuine failure forever.

**Fixtures come from the publisher.** A secret scanner and a content policy
share nothing but the interface, so the suite cannot invent a payload that
should be refused. A benign payload is *required*: without it, a guardrail that
denies everything passes every assertion about denying and refuses all real
traffic. A component supplying no trigger is taken at its word that it only
allows, and the report says so rather than asserting a behaviour never claimed.

**The sidecar protocol is JSON over a Unix socket**, matching the NER sidecar
rather than introducing gRPC for one more thing. Same reasoning as ADR 0008:
this crosses a language boundary between processes deployed together, not a
version boundary, and being able to curl the socket at three in the morning is
worth more than the wire efficiency. An unknown verdict is an error rather than
an allow — when the two sides disagree about the protocol, resolving it in
favour of letting the request through is the wrong default for a component
whose job is to refuse things.

## The boundary, precisely

The sandbox is a container with no network, a read-only root, a tmpfs for
`/tmp`, all capabilities dropped, `no-new-privileges`, a non-root user, memory
and CPU and pid caps, and a wall-clock deadline enforced by the runner rather
than by the runtime — a container told to stop is a container that might not.

That stops the ordinary failures: a component that dials out to fetch its real
payload, that writes to the host, that forks until the box falls over, that
never exits.

It is **not** a defence against a kernel escape. A container shares a kernel,
so a component carrying a local privilege-escalation exploit is not contained
by any flag in that file. `--runtime` exists for exactly this: a deployment
admitting genuinely untrusted third-party code points it at gVisor or Kata,
which present the same command line. The package does not hardcode which.

The isolation flags are asserted against the built argv rather than against
observed container behaviour. A test that needs a container runtime installed
is a test that does not run, and a dropped flag would then survive review and
ship. `scripts/admission-check.sh` runs the real thing where a runtime is
available, which is what catches a flag that is *wrong* rather than missing.

**The component runs as the runner's own uid.** A Unix socket needs write
permission to connect, and one created under the default umask by a different
user gives the runner none — verified rather than assumed: same uid connects,
a different uid gets `EACCES`. The alternative is a component that makes its
socket world-writable, which is a worse trade than an unprivileged uid the host
already runs as. When the runner is root it falls back to nobody, because
running the container as root just because the runner is root would hand a
container escape the host's root.

**Reachability is checked before the battery, and its failure is an error
rather than a verdict.** The socket existing only means the component created
the file. If nothing accepts a connection, every case fails with the same dial
error and the report reads as though the component were broken — when the
mount, the runtime or the host is just as likely. Docker Desktop on macOS does
exactly this: its bind mount crosses a VM boundary that does not proxy Unix
sockets, so the socket appears on the host and connecting is refused.

## Why not the alternatives

**Run the suite in the control plane, sandboxed by convention.** There is no
such sandbox. A Python process with an event loop and a database handle is not
an isolation boundary, and calling it one is worse than admitting there is
none.

**Let the runner activate directly.** It is the component with the largest
attack surface in the system — by design, since executing untrusted code is its
job. Giving the most exposed component the authority to grant production access
inverts the whole arrangement.

**Have the component declare its own name.** A snapshot binds by name. Asking
an untrusted process what it is called lets it answer with someone else's
binding, so the name comes from the registration.

## Consequences

- Only the guardrail port has a sandboxed suite. Provider and KV suites exist
  but need a live upstream or a real store, which is a different sandbox shape;
  the runner refuses those ports rather than running an empty battery and
  reporting a pass.
- The suite version is recorded on every admission. A suite that gains a case
  is a new version, and components admitted under the old one are visibly
  admitted under an older bar rather than silently grandfathered.
- `scripts/admission-check.sh` proves the loop closes with a stubbed runtime,
  because the wiring and the isolation flags fail in different ways and are
  worth checking separately.
