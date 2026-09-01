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
  internal/wire/      Snapshot wire format and its mapping to domain types.
  internal/gateway/   The request path, transport-free: auth, admit, route, adapt.
  internal/httpapi/   HTTP transport. Knows about status codes; decides nothing.
  internal/secrets/   Credential resolution. Secrets never enter a snapshot.
  internal/telemetry/ Usage and audit events, off the request path.
  internal/tracing/   OpenTelemetry setup. Adopted, not wrapped.
  internal/limits/    Rate limiting: a local lease in front of a shared window.
  internal/router/    Selection and execution: candidates, breakers, failover.
  internal/guardrails/ Inspections, run under the budget each was admitted with.
  internal/policy/    The compiled decision table. Evaluated, never parsed.
  cmd/gateway/        The worker binary.
  cmd/snapshotgen/    Writes a demo snapshot, so the repo runs without a control plane.
  internal/contracts/ Per-port contract suites; also the plugin admission gate.
  internal/adapters/  Concrete port implementations.
controlplane/       Python. Registry, identity, policy authoring, snapshot compilation.
  domain/             Frozen dataclasses. What the control plane reasons about.
  db/                 SQLAlchemy models, Alembic migrations, the repository.
  snapshot/builder.py Domain -> the versioned artifact the data plane serves.
  wire/               Generated protobuf, shared with the data plane.
  service/            Key lifecycle and accounting. Testable without HTTP.
  api/                The admin API. Translation only.
  cli.py              gatewayctl.
proto/              The snapshot schema. One definition, generated for both.
examples/           A demo snapshot description.
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

## Try it

```bash
make demo
```

That writes a demo snapshot with one echo provider and one API key, then runs
the gateway on `:8080`. In another terminal:

```bash
curl -s localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer gw_demo_demo-secret' \
  -d '{"model":"fast","messages":[{"role":"user","content":"hi"}]}'
```

To point it at a real OpenAI-compatible endpoint instead:

```bash
cd dataplane && go run ./cmd/snapshotgen -out ../snapshot.pb \
  -pepper "$GATEWAY_KEY_PEPPER" \
  -endpoint https://api.openai.com/v1 -model gpt-4o-mini
```

That registers a second deployment under the alias `real`, with its credential
resolved from `OPENAI_API_KEY` at call time rather than baked into the snapshot.
Add `"stream": true` to the request for server-sent events.

`-anthropic-endpoint https://api.anthropic.com/v1` does the same for the
Messages API under the alias `reasoning`, served on `POST /v1/messages`. The
gateway does not translate between the two schemas: a model is served on the
surface its adapter speaks, and asking for it on the other one is a 404 that
says so.

The echo provider returns the request back, which is the point: the whole path —
key authentication, model allowlist, budget check, trust-tier filtering, routing
and the provider call — runs with no network and no database. `curl
localhost:8080/readyz` reports which snapshot version is serving.

## Working on it

```bash
make check
```

Runs the cross-language contract check, gofmt, `go vet`, `go test -race`, ruff, mypy `--strict` and pytest — the
same flags CI uses. `make help` lists the rest.

Requirements: Go 1.26+, Python 3.12+, [uv](https://docs.astral.sh/uv/).

Postgres and Redis are optional locally — the suites fall back to SQLite and an
in-process store — but they are what CI gates on, so it is worth running them
before pushing anything that touches either:

```bash
docker run -d --name gw-redis -p 6380:6379 redis:7
docker run -d --name gw-postgres -p 5433:5432 \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=gateway_test postgres:17

export GATEWAY_TEST_REDIS_URL=redis://127.0.0.1:6380
export GATEWAY_TEST_DATABASE_URL=postgresql+asyncpg://postgres:postgres@127.0.0.1:5433/gateway_test
make check
```

The fallbacks exist so a clone runs with nothing installed, not so the real
thing can be skipped: an emulator is a reimplementation, and reimplementations
differ.

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) covers setup, the invariants that are not
negotiable, and what a reviewable PR looks like.
Security issues go through [SECURITY.md](SECURITY.md), never a public issue.

Licensed under [Apache 2.0](LICENSE).

## Status

Under construction, one module at a time. See `docs/adr/` for what has been
decided and why.

| Module | State |
|---|---|
| M0 — Foundation: vocabulary, ports, layered snapshot | **done** |
| M1 — Snapshot: schema, holder, atomic swap, N−1 rollback, file source | **done** |
| M2 — Data-plane vertical slice: key auth, `/v1/chat/completions`, echo provider | **done** |
| M3 — `ProviderPort` for real: OpenAI-compatible adapter, SSE streaming, `SecretsPort` | **done** |
| M3b — Anthropic adapter and `/v1/messages` | **done** |
| M4 — Usage events, `TelemetryPort`, Prometheus, cost attribution | **done** |
| M4b — OTel spans and OTLP export | **done** |
| M5a — Control plane: domain model, snapshot builder, `gatewayctl` | **done** |
| M5b — Postgres schema, migrations, repository | **done** |
| M5c — Admin API, key rotation | **done** |
| M5d — Snapshot subscriber: worker fetches from the control plane | **done** |
| M6a — Rate limits: local lease bucket, enforced at admission | **done** |
| M6-pricing — Token classes and cost/price separation | **done** |
| M6b — Redis `KVStore`, making limits fleet-wide | **done** |
| M6c — Budgets: accounting consumer folds spend into snapshots | **done** |
| M7a — Router: selection/execution split, circuit breakers, passive health | **done** |
| M7b — Active health probing, cost and locality objectives | **done** |
| M8a — `GuardrailPort`: chain, budgets, secret scan, injection alerts | **done** |
| M8b — Policy engine: compiled decision table, geo and IP rules | **done** |
| M9 — PII chain | next |
| M10 — Component registry | |
| M11+ — Fine-tuning | |
