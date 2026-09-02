# ADR 0012 — Publisher signatures, with the trust root outside the database

**Status:** accepted · 2026-09-02
**Extends:** [ADR 0009](0009-component-registry.md), [ADR 0010](0010-sandboxed-admission-runner.md)

## Context

The registry binds an admission record to a manifest digest, so a component
edited after it passed no longer matches what passed. That answers *are these
the bytes that were tested*.

It does not answer *who submitted them*. Today anyone who can reach the admin
API can publish a component under any name, and the only record of where it
came from is a row someone wrote. For a system whose whole job is executing
other people's code in the request path, "the database says so" is not an
adequate answer to that question.

## Decision

**Ed25519 detached signatures over the manifest digest.** Small keys, no
parameter choices to get wrong, and the digest already exists and is already
what an admission binds to — so a signature and an admission cover exactly the
same bytes without either having to know about the other.

**Domain-separated.** The signed payload is `model-gateway/manifest/v1:` +
digest. Ed25519 signs whatever bytes it is given, so a signature over a bare
hash is valid in any other protocol that signs bare hashes with the same key.
One function produces the payload, used by both the signer and the verifier, so
the two cannot drift.

**The trust root is a file, not a table.** This is the decision the rest hangs
on. Keys are loaded from `GATEWAY_TRUSTED_KEYS` at startup. An attacker who
owns the database can set a component's status to active and can write whatever
they like into its signature columns — but they cannot make those bytes verify
against keys they did not get with the database.

A trust root stored beside the thing it is protecting protects nothing.

**Verified twice, and the second one is the point.** Registration verifies so
that a publisher gets a clear error while they are watching. The snapshot
builder verifies *again*, re-deriving the answer from the configured keys
before binding any component. Without the second check, "verified at
registration" is a fact about a row — and rows are what an attacker with a
database has. The stored signature is evidence to be re-checked, never a claim
to be believed.

**A signature nobody can check is not a valid one.** A control plane with no
keys configured still registers *unsigned* components, but a *signed* one fails
loudly. Treating an uncheckable signature as absent would turn a
misconfiguration into a silent downgrade, which is the failure mode this whole
module exists to prevent.

**Three key states, because rotation and compromise are different events.**

| | |
|---|---|
| `active` | May sign new registrations |
| `retired` | May not sign anything new; what it already signed stays valid |
| `revoked` | Distrusted entirely — nothing it signed verifies anywhere |

Retirement is planned rotation: invalidating everything a key ever signed would
mean re-signing the whole registry to replace one key. Revocation is
compromise, and it is deliberately blunt — a revoked key's components stop
verifying, so they leave the fleet on the next snapshot build rather than
whenever an operator remembers to retire them one at a time.

**Optional by default.** `GATEWAY_SIGNATURE_POLICY=required` turns enforcement
on. Defaulting to required would lock every already-registered component out of
every future snapshot the moment this shipped, and a security control that
breaks the system on upgrade is one that gets reverted rather than adopted.
Requiring signatures with no keys configured is refused at startup, because
otherwise every registration and every snapshot fails in a way that looks like
the software being broken.

## Why not the alternatives

**Store trusted keys in the database, managed through the admin API.** Ergonomic,
and it defeats the entire mechanism: the signature would then prove only that
the database agrees with itself.

**Sign the whole manifest rather than its digest.** The digest is already the
canonical, order-independent serialisation, and it is already what an admission
binds to. Signing a second serialisation would create two canonical forms and a
question about which one a mismatch means.

**Verify only at registration.** Cheaper, and it protects against the wrong
threat. A publisher submitting a bad signature is a mistake; an attacker who
already has the database is the case worth designing for, and that attacker
does not go through registration.

**Verify in the data plane.** A worker would need the public keys, and the
snapshot it receives is already sealed with a digest by a control plane it
trusts. Adding a second trust root to every worker widens the blast radius of a
key leak for no gain.

## Consequences

- `cryptography` is a new control-plane dependency. Python has no Ed25519 in
  the standard library, and it is the maintained option.
- Publishers need tooling, so `gatewayctl keygen` and `gatewayctl sign-manifest`
  exist. Keygen refuses to overwrite an existing key file and never prints the
  private half.
- Revoking a key is an outage for its components, by design. That is what
  revocation means; the escape hatch is retirement.
- The trusted-keys file must be no more writable than the process reading it.
  If whatever compromises the database can also write that file, this buys
  nothing — and there is no way to make it buy something.
- The data plane is untouched. Signatures gate what enters a snapshot; a worker
  keeps trusting the sealed snapshot it is given.
