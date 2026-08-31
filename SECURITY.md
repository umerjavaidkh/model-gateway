# Security policy

## Threat model, stated plainly

A model gateway holds every provider credential in an organisation. **Threat-model
it as a secrets broker, not as a proxy.** Comparable projects in this space have
shipped a supply-chain compromise, a critical SQL injection exploited within 36
hours of disclosure, and a Host-header authentication bypass — all in 2026. The
assumption here is that this project is as attackable as those, and that the
difference is in what we check rather than in who we are.

## Reporting a vulnerability

**Do not open a public issue.** Use GitHub's private reporting:
[Report a vulnerability](https://github.com/umerjavaidkh/model-gateway/security/advisories/new).

Please include a description, the affected version or commit, reproduction steps
or a proof of concept, and what an attacker gains. You will get an
acknowledgement within 72 hours and an assessment within 7 days. Coordinated
disclosure is preferred; we will credit you unless you ask otherwise.

## In scope

Anything that lets a caller cross a boundary the gateway is supposed to enforce:

- Authentication or authorisation bypass — reaching another tenant's principals,
  budgets, snapshot layer or provider credentials
- Credential exposure — a provider secret reaching a snapshot, a log, a metric,
  an error message, or a telemetry sink
- Snapshot integrity — installing a stale, forged or unsigned snapshot; version
  rollback that reinstates revoked keys or refunds spent budget
- PII leakage — unredacted payload reaching an external trust tier, or the token
  vault being readable across tenants
- Injection of any kind into the control plane's admin API
- Plugin sandbox escape, or a registry component executing outside its declared
  resource limits
- Denial of service that a rate limit should have contained

## Out of scope

- Prompt injection against an upstream model. The design ships injection
  detection as **logging, not a blocking control**, with a stated accuracy
  caveat — it is largely ineffective against a determined attacker and claiming
  otherwise would be the actual security problem.
- Findings in a provider's own API.
- Missing hardening with no demonstrated impact, and automated-scanner output
  submitted without a working proof of concept.

## What runs on every change

| Control | Where |
|---|---|
| CodeQL `security-and-quality`, Go and Python, plus a weekly scan | `.github/workflows/codeql.yml` |
| `gosec` and correctness linters as inline review comments | `.github/workflows/review.yml` |
| Secret scanning with push protection | Repository setting |
| Dependabot for gomod, uv and GitHub Actions | `.github/dependabot.yml` |
| Branch protection: no direct pushes to `main`, all checks required | Repository setting |

Workflows declare least-privilege `permissions:` blocks. A change to
`.github/workflows/` is a security change and is reviewed as one.

## Supported versions

Pre-1.0 and under active construction. Only `main` is supported.
