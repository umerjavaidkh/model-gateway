# Running the whole thing locally

```bash
make local-up      # build, start, migrate, seed
make local-smoke   # prove it works
make local-logs    # follow everything
make local-down    # remove it, volumes and all
```

Then:

```bash
curl -s localhost:18080/v1/chat/completions \
  -H 'Authorization: Bearer gw_demo_local-development-key' \
  -d '{"model":"fast","messages":[{"role":"user","content":"hello"}]}'
```

| | |
|---|---|
| `localhost:18080` | worker A |
| `localhost:18090` | worker B |
| `localhost:18081` | admin API |

## Why containers rather than running it natively

Because macOS is not Linux, and the differences are not theoretical — they
have bitten this project twice already:

- **Docker Desktop cannot proxy a Unix socket across a bind mount.** The
  sandboxed admission runner falls back to a stub on macOS for exactly this
  reason (`scripts/admission-check.sh` detects it and says so). Inside Linux
  containers it works.
- **macOS caps Unix socket paths at about 100 bytes.** Tests that bind sockets
  in temp directories had to be written around it.

Running natively also runs one worker, against SQLite or a hand-started Redis,
as your own user, with your own filesystem. None of those is what ships.

Here, every process is a Linux container with the uids, the filesystem, the
network and the socket semantics the deployment will have.

## What this actually exercises

Things that cannot be wrong in a unit test because they do not exist there:

- **Configuration crossing a network.** Workers bootstrap from the admin API
  and poll it; `local-smoke` builds a snapshot and asserts both workers apply
  it without restarting.
- **Spend crossing two systems.** Worker → Redis stream → accounting consumer →
  Postgres, in four separate containers.
- **A fleet rather than a process.** Two workers, independent circuit breakers,
  rate limits shared through Redis rather than held in memory.
- **A sidecar over a real Unix socket**, one per worker, sharing only that
  worker's volume — which is the production shape and the thing that caught
  the uid mismatch below.
- **Real Postgres and real Redis**, not SQLite and not miniredis.

It has already found one production bug that every test missed: the accounting
consumer crashed on its first idle five-second window, because redis-py raises
on a blocking read that finds nothing. No test caught it because a test always
has events waiting. A quiet Redis is what production looks like at four in the
morning.

## What it still cannot tell you

Be honest about this list. A green smoke run does not mean production is fine.

- **No real providers.** The seeded model is the in-process echo adapter. Real
  provider latency, rate limits, partial failures and streaming quirks are not
  here. Point a deployment at a real endpoint and set a credential to test
  those, and expect to find things.
- **No GPUs, so no real multi-LoRA.** Adapter routing is exercised as routing;
  whether vLLM loads an adapter under load is not.
- **arm64, not amd64.** Your M1 runs a different architecture from most
  production clusters. Go and Python hide this well; a CGo dependency or a
  provider SDK wheel would not. `docker compose build --platform linux/amd64`
  runs under emulation, slowly, if you need to check.
- **No Kubernetes.** No pod scheduling, no rolling deploys, no network policy,
  no service mesh, no real load balancer in front of the workers.
- **One machine.** No cross-AZ latency, no partitions, no clock skew, and
  contention that only appears at real concurrency.
- **Traffic volume.** Two workers on a laptop will not surface the things that
  only appear at production request rates.

## Credentials

The pepper, the admin token and the seeded key are in `compose.yaml` in clear
text on purpose: this stack is disposable and nothing in it should ever be
reachable from anywhere. **Do not copy this file into anything real.** In a
real deployment the pepper comes from a secret manager, the admin listener is
behind mTLS, and the seeded key does not exist.

## Adding a real provider

```bash
docker compose exec -T postgres psql -U gateway -d gateway -c "
  insert into deployments (id, base_model, provider, endpoint, trust_tier, weight,
                           credential_ref, input_cost_micro_usd, output_cost_micro_usd)
  values ('openai-1', 'gpt-4o-mini', 'openai-compatible', 'https://api.openai.com/v1',
          1, 100, 'env:OPENAI_API_KEY', 150, 600);"
```

Then rebuild the snapshot (`POST /v1/snapshots`) and give the workers the
credential. Trust tier 1 is external, which means the PII chain will transform
payloads on the way out — which is the point, and worth watching happen.
