# Contributing

## The short version

One module at a time, each a small reviewable PR. `make check` before you push.
Explain *why* in the commit message; the code already says what.

## Getting set up

Requirements: Go 1.26+, Python 3.12+, [uv](https://docs.astral.sh/uv/).

```bash
make check
```

That runs gofmt, `go vet`, `go test -race`, golangci-lint, ruff, mypy `--strict`
and pytest — the same flags CI uses. A check that only exists in CI is a check
nobody can fix, so anything CI runs, `make check` runs.

Optional, and worth it — the Go linter:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

## Ground rules that are not negotiable

**`dataplane/internal/core` imports nothing.** Not the rest of the module, not
anything outside the standard library. This is what keeps "every component is
replaceable through a registry" true in a year rather than only on day one. If a
change to `core` needs an import, the change belongs somewhere else.

**The data plane never touches a database.** It holds no durable state and
serves every request from its in-memory snapshot. This is the property that lets
a control-plane outage degrade to "configuration is frozen" instead of "traffic
stops". `StorePort` deliberately has no SQL sub-interface so that this is
structural rather than a convention.

**Snapshot-resident values are immutable.** Anything reachable from a
`core.Snapshot` is built once and read concurrently by every in-flight request.
Accessors that return slices return snapshot-owned memory — read it, never write
to it.

**Every request takes exactly one lease.** `defer lease.Release()` immediately
after `Acquire`. A leaked lease pins a snapshot generation, and its plugins, for
the life of the process.

**No blocking work in the request path** that is not budgeted. Guardrails
declare a timeout and a failure mode in their manifest, and the caller enforces
it rather than trusting the implementation.

## Style

Match the surrounding code. Beyond that:

- **Comments say why, not what.** A comment restating the line below it is noise;
  a comment explaining why the obvious approach was rejected is the most valuable
  thing in the file.
- **Name the trade-off out loud.** A senior change says what it gives up. Silent
  trade-offs are how a codebase becomes unmaintainable.
- **Validate at construction, not at use.** A value that exists should be
  coherent, so the request path carries no defensive checks.
- **Errors carry a `core.Code`.** Wrap foreign errors at the boundary of the
  adapter that produced them, so no caller type-switches on a vendor's error.
- **Dependencies are arguments.** A component that constructs its own client
  cannot be tested without one. Python: no module globals, no `monkeypatch` on
  our own internals — that is the tell that something should have been a
  parameter.

## Tests

Test behaviour, not implementation. A test whose name describes a property
(`TestALeaseKeepsItsVersionAfterASwap`) survives a refactor; one named after a
method does not.

- **Concurrency changes need a `-race` test.** The snapshot holder's whole design
  is concurrent reads of immutable state; a data race there is the one bug class
  we cannot ship.
- **New port implementations run the contract suite** in
  `dataplane/internal/contracts/`. That suite is also the registry's admission
  gate, so passing it is what makes a component installable.
- **A performance claim needs a benchmark** in the repo. Restating an aspirational
  number is how every vendor in this market lost credibility.

## Commits and PRs

- One cohesive concern per PR. If the description needs the word "also" twice,
  it is two PRs.
- Commit messages: a short subject, then the reasoning. Reviewers read the body
  to decide whether the approach is right, not to learn what changed.
- A departure from [`model-gateway-architecture.md`](model-gateway-architecture.md)
  needs an ADR in [`docs/adr/`](docs/adr/) explaining what and why. Departing is
  fine and expected — doing it silently is not.
- All checks must pass and conversations must be resolved before merge.

## Reporting a security issue

Do not open an issue. See [SECURITY.md](SECURITY.md).
