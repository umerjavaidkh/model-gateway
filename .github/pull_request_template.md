## What and why

<!-- What changed, and the reasoning a reviewer needs to judge the approach.
     The diff already says what; this should say why. -->

## Design decisions

<!-- Anything a reviewer would otherwise have to reverse-engineer: an approach
     considered and rejected, a trade-off accepted, a deviation from
     model-gateway-architecture.md (which needs an ADR in docs/adr/). -->

## Testing

<!-- How you verified it. `make check` output, and anything it does not cover. -->

## Checklist

- [ ] `make check` passes
- [ ] Tests cover the behaviour, not the implementation
- [ ] Concurrency changes have a `-race` test
- [ ] New port implementations run the contract suite
- [ ] Performance claims are backed by a benchmark in the repo
- [ ] `internal/core` still imports nothing outside the standard library
- [ ] Deviations from the reference architecture are recorded in `docs/adr/`
- [ ] No secret, credential or provider key added to code, config or fixtures
