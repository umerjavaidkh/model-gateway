# PII NER sidecar

Finds names, locations and organisations — the entities the gateway's
deterministic tier cannot, because they have no shape to match.

Runs beside a gateway worker and answers over a Unix socket. It is never
reachable over the network: its input is unredacted personal data.

The reasoning is in [ADR 0008](../../docs/adr/0008-statistical-detection-in-a-sidecar.md).

## Running it

```bash
uv run pii-ner
```

`PII_NER_SOCKET` sets the socket path (default `/tmp/pii-ner.sock`), and the
worker is pointed at the same path:

```bash
GATEWAY_NER_SOCKET=/tmp/pii-ner.sock
```

The worker health-checks the socket at startup and refuses to start if nothing
answers, so a wrong path fails the deploy rather than every classified request
after it. A worker with no `GATEWAY_NER_SOCKET` starts normally and serves
everything except the rules that require deep inspection.

## The backend

`PII_NER_BACKEND=patterns` (the default) uses the per-language recogniser
registry in `recognizers.py`. **It is a placeholder for the statistical tier,
not NER** — an honest set of patterns over honorifics, organisation suffixes
and location prepositions. It exists so the boundary, the byte offsets and the
failure modes are real and tested.

Installing the `presidio` extra is the intended production backend:

```bash
uv sync --extra presidio
```

Recognisers are registered per language for a reason: an English model misses
Arabic entities almost entirely, and misses them silently. An unknown language
falls back to the default set rather than returning nothing.

## The contract

`POST /v1/detect` takes `{"text": ..., "language": ...}` and returns entities
at **byte** offsets into the UTF-8 encoding of the text. The Go client verifies
every span against the payload and drops the ones that do not match, so an
offset regression stops protecting requests rather than corrupting them —
`tests/test_app.py` is where the two languages agree about this.

## Checks

```bash
make check-ner
```
