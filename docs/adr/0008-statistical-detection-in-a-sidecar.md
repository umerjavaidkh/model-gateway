# ADR 0008 — Statistical PII detection runs in a sidecar, opt in per rule

**Status:** accepted · 2026-09-02
**Extends:** [ADR 0007](0007-pii-transform-per-attempt.md)

## Context

The deterministic tier finds what has a shape: card numbers, IBANs, Emirates
IDs, email addresses. It cannot find a person, an employer, or a city, because
those have no shape — "Ada Lovelace" and "Ada Motors" are the same string to a
regular expression, and only one of them is a person.

Names, locations and organisations are the majority of what a data protection
review actually asks about, so a PII chain without them protects the easy half.
Finding them needs a statistical model, and a statistical model brings three
things the data plane has spent this project avoiding: Python, hundreds of
megabytes of model weights, and a latency that is measured in tens of
milliseconds rather than microseconds.

## Decision

**A sidecar, not a library.** The models are Python; embedding them would mean
either a CGo bridge or rewriting the data plane's language choice around one
feature. The sidecar also scales independently of the request path, and carries
its own CVE surface in its own image — a spaCy advisory should not force a
gateway redeploy.

**A Unix socket, not a port.** The sidecar's entire input is unredacted
personal data. A localhost port is reachable by every other container in the
pod and by anything outside it after one misconfiguration; a socket is reachable
by the process holding the file descriptor.

**JSON, not protobuf.** The snapshot is protobuf because it crosses a language
boundary *and* a version boundary — an old worker must read a new snapshot.
This crosses only a language boundary, between two processes deployed together
at one version, and the message has two fields. JSON can be inspected with curl
over the socket when something is wrong at three in the morning.

**Opt in per policy rule, not derived from the data class.** Deep inspection
costs tens of milliseconds. A rule asking for it is an operator deciding that
this traffic is worth that; deriving it from the class would silently apply the
cost to every request a broad classification touches.

**The offsets are bytes, and the client verifies them.** Python indexes
characters and Go indexes bytes, so a single emoji ahead of a name puts the two
four apart. A span that does not match the payload is dropped rather than
trusted, because substituting on a bad offset corrupts the request at a
position nobody chose — worse than missing the entity.

**Unavailability is fail-closed only where the class demands it.** If the tier
that a rule asked for did not run — the sidecar failed, or the worker has none
configured — a request whose class calls for tokenisation is refused, because
that class exists precisely for data whose values matter. Anything lower
proceeds on the deterministic result and logs. Refusing everything would turn
one sidecar restart into a fleet-wide outage; refusing nothing would make a
missing `GATEWAY_NER_SOCKET` a silent downgrade across the fleet.

## Why not the alternatives

**Call a hosted PII API.** A network hop to a third party, carrying exactly the
data the chain exists to keep from third parties.

**Run detection in the control plane.** Detection is per request; the control
plane is per configuration change. This is the same mistake as calling out to a
policy service in the hot path.

**Make it a `GuardrailPort`.** Guardrails decide whether a request proceeds.
This decides what the request *says* — it feeds the transform, and it must run
before the payload is built for a given deployment, not alongside the checks.

## Consequences

- A worker without a sidecar is a valid deployment. It says so once at startup
  rather than per request, and refuses only the requests whose class requires
  the tier it does not have.
- A wrong socket path fails the worker at startup, not silently at request
  time.
- The pattern backend shipped here is a placeholder for the statistical tier,
  not NER. It exists so the boundary, the offsets and the failure modes are
  real and tested; swapping in Presidio is a change to one module.
