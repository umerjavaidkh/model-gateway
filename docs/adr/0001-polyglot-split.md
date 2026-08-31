# ADR 0001 — Go data plane, Python control plane

**Status:** accepted · 2026-08-31
**Supersedes:** the reference plan's open decision in §8 ("Data plane language")

## Context

The reference architecture recommends Go for the data plane and leaves the choice
open. The project was initially scoped as Python-only; that constraint was lifted
in favour of choosing per component.

## Decision

| Component | Language |
|---|---|
| Data plane — auth, admit, route, adapt | **Go** |
| Control plane, admin API, usage accounting | **Python** (FastAPI) |
| Provider translation | **LiteLLM Rust core**, adopted as a dependency behind `ProviderPort` |
| Guardrail and PII sidecars | **Python** over a Unix domain socket |
| Trainer and eval reconcilers | **Python** |
| Admin console | **TypeScript** |

We write no Rust of our own.

## Why not Python for the data plane

Python is viable — the plan itself argues that gateway overhead is a rounding
error next to provider TTFT, and that what matters is how much work runs
synchronously in the request path. But it forced three concessions that all
disappear in Go:

1. **No in-process WASM plugins.** There is no Python equivalent of wazero's
   maturity, so every third-party component would have to be out-of-process.
2. **Rate-limit lease multiplication.** CPython needs N worker processes per pod,
   each taking its own token-bucket lease, so over-admission scales with process
   count and the snapshot is duplicated N times per pod.
3. **Roughly 5x worse p99** and a third of the throughput per pod.

## Why Python everywhere else

The control plane is off the request path by design; a control-plane outage
degrades to "configuration is frozen". Its availability and latency requirements
are an order of magnitude looser than the data plane's, and iteration speed
matters more. Everything it integrates with — Presidio, LLaMA-Factory, torchtune,
eval harnesses — is Python already.

## Consequences

- The snapshot is a cross-language contract. It must be defined once and
  generated for both sides, not hand-maintained twice. See ADR 0003.
- The error taxonomy is currently mirrored by hand in `internal/core/errors.go`
  and `model_gateway_control/errors.py`. This duplication is accepted only until
  the shared schema lands.
- Two toolchains in CI. `make check` runs both with the same flags CI uses.
