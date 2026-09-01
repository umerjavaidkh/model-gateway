# ADR 0004 — Native Go provider adapters, not a LiteLLM sidecar

**Status:** accepted · 2026-09-01
**Amends:** reference plan §2 ("Provider translation — **Adopt** LiteLLM")

## Context

The plan's adopt/build table is emphatic and, on its own terms, right:

> Provider translation — **Adopt** — LiteLLM Rust core as a library behind
> `ProviderPort`. 140 providers, day-0 model support, maintained elsewhere.
> Zero differentiation, infinite maintenance.

That reasoning assumed a Python data plane, where LiteLLM is `pip install` and
a function call. [ADR 0001](0001-polyglot-split.md) put the data plane in Go,
which turns the same decision into something different: a Python process in the
request path, spoken to over a socket.

## Decision

Write provider adapters natively in Go, starting with two:

- **`openaicompat`** — the OpenAI chat-completions schema. One adapter serves
  OpenAI, Azure OpenAI, vLLM, TGI, Ollama, Groq, Together, Fireworks,
  DeepInfra, and every self-hosted server that speaks it. This is the majority
  of real traffic and all of the self-hosted tier.
- **`anthropic`** — the Messages API, which is different enough to need its own.

A LiteLLM sidecar remains available later for the long tail, behind the same
`ProviderPort`. That is precisely what the port is for, and adding it changes
no calling code.

## Why not the sidecar now

**It reverses the language decision for the hot path.** ADR 0001 chose Go for
the request path on latency and deployment grounds. Routing every provider call
through a Python process reintroduces what that decision avoided, for the
component where it matters most.

**The second runtime is not free.** A Python sidecar is another image to build,
another dependency tree to patch, another process to supervise, and another
thing to debug at three in the morning.

**The plan's own security section argues against it.** It documents a
supply-chain compromise, a critical SQL-injection CVE exploited within 36 hours
of disclosure, and a Host-header auth bypass — all in LiteLLM, all in 2026. A
gateway holds every provider credential in the organisation. Putting that
codebase in the request path is a considered risk, not a free adoption.

## What it costs, stated plainly

- **No day-0 support for exotic providers.** A new provider with a bespoke API
  needs an adapter written. The OpenAI-compatible schema absorbs most of them,
  but not all.
- **Two adapters to maintain**, including their streaming edge cases. This is
  real work that LiteLLM would have done for us.
- **The plan's "infinite maintenance" warning is not wrong** — it is a bet that
  two adapters covering most traffic is less total cost than a Python runtime
  in the hot path. If the long tail turns out to matter, the sidecar is the
  answer and the port already accommodates it.
