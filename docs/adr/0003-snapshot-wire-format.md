# ADR 0003 — Snapshot wire format is deferred to M1, and will be generated

**Status:** accepted · 2026-08-31 · implemented in M2a (proto3, not editions:
protoc 25.x treats editions as experimental)

## Context

The control plane (Python) compiles snapshot layers; the data plane (Go) consumes
them. That makes the snapshot a cross-language contract, and a hand-maintained
contract in two languages drifts — silently, and in the component whose entire
job is to be the single source of configuration truth.

## Decision

Two separate things, deliberately kept apart:

- **Domain types** (`internal/core`) are Go-native and hand-written. They are
  shaped for the request path — indexed maps, validated invariants, immutability
  — and they are nobody's wire format.
- **The wire format** will be Protobuf, defined once in `proto/gateway/v1/`, and
  generated for both languages. It lands in **M1**, together with the snapshot
  holder and subscriber that actually need it.

A mapping layer converts between the two at the module boundary.

## Why not define the proto now

M0's job is to settle the vocabulary. Adding codegen tooling — `buf`, two plugin
chains, a generated-code check in CI — before anything transmits a snapshot would
make this PR larger without making the design more certain, and the wire format
should be shaped by the subscriber that consumes it rather than guessed at.

## Why domain types and wire types stay separate

They have different jobs and different change rates. Wire types are append-only
and backward-compatible forever; domain types are refactored freely. Collapsing
them means every internal rename is a wire-compatibility question, which is how
projects end up unable to rename anything.
