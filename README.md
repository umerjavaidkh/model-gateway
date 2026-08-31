# Model Gateway

A multi-tenant AI/LLM gateway: one OpenAI-compatible endpoint that every app,
agent and notebook calls instead of calling providers directly. It exists to be
the single enforcement point for the four things that are otherwise duplicated in
every service — provider credentials, tenant identity, spend, and the model
catalog.

The design is documented in [`model-gateway-architecture.md`](model-gateway-architecture.md).
Decisions that depart from it are recorded in [`docs/adr/`](docs/adr/).

## The central idea

The control plane compiles configuration into **immutable, versioned snapshots**.
The data plane serves every request from a snapshot held in memory, holds no
durable state, and reads no database.

The property this buys is worth stating plainly: **a control-plane outage
degrades to "configuration is frozen", not "traffic stops".** That is why the
control plane can run at lower availability than the data plane — and why it can
be written in Python while the data plane is written in Go.

## Layout

```
dataplane/          Go. The request path: auth -> admit -> route -> adapt.
  internal/core/      Domain vocabulary: types, errors, ports, snapshot.
  internal/snapshot/  Worker-side holder: what the current configuration is.
  internal/contracts/ Per-port contract suites; also the plugin admission gate.
  internal/adapters/  Concrete port implementations.
controlplane/       Python. Registry, identity, policy authoring, snapshot compilation.
docs/adr/           Architecture decision records.
```

**`internal/core` imports nothing** — not from the rest of this module, not from
outside the standard library. That rule is what keeps "every component is
replaceable" true a year from now rather than only on day one.

## Performance targets

The reference plan targets < 2 ms p99 gateway overhead and 10k RPS per worker
pod. We have not measured ourselves against those yet and will publish real
numbers from a load harness checked into this repo rather than restating
aspirational ones. What we will hold to is the architectural constraint behind
them: **the amount of work that runs synchronously in the request path**.

## Working on it

```bash
make check
```

Runs gofmt, `go vet`, `go test -race`, ruff, mypy `--strict` and pytest — the
same flags CI uses. `make help` lists the rest.

Requirements: Go 1.26+, Python 3.12+, [uv](https://docs.astral.sh/uv/).

## Status

Under construction, one module at a time. See `docs/adr/` for what has been
decided and why.

| Module | State |
|---|---|
| M0 — Foundation: vocabulary, ports, layered snapshot | **done** |
| M1 — Snapshot holder: atomic swap, lease-based drain, N−1 rollback | **done** |
| M2 — Snapshot wire format and subscriber | next |
| M2 — Data-plane vertical slice | |
| M3 — `ProviderPort` for real: LiteLLM, streaming | |
| M4 — Usage events and telemetry | |
| M5 — Control plane: Postgres, identity, snapshot builder | |
| M6 — Rate limits and budgets | |
| M7 — Router: selection, execution, circuit breakers | |
| M8 — Guardrails and policy | |
| M9 — PII chain | |
| M10 — Component registry | |
| M11+ — Fine-tuning | |
