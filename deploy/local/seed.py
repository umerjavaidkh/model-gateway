"""Seed a local fleet: one tenant, one key, a model with two deployments.

Idempotent, because compose restarts things and a seed that fails the second
time makes `up` look broken. Everything here is the same shape the checks use,
so what runs locally is what the tests already assert about.
"""

from __future__ import annotations

import asyncio
import os
import sys

from sqlalchemy import select

from model_gateway_control.db import models
from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.domain.identity import compute_key_lookup

TENANT = "demo"
PREFIX = "demo"
#: Printed, never stored. What the control plane keeps is the HMAC lookup,
#: which is useless without the pepper.
SECRET = "local-development-key"
#: How a key is presented: gw_<prefix>_<secret>. The prefix is what routes the
#: lookup to a tenant before anything is verified.
PRESENTED = f"gw_{PREFIX}_{SECRET}"


async def main() -> int:
    database_url = os.environ["GATEWAY_DATABASE_URL"]
    pepper = os.environ["GATEWAY_KEY_PEPPER"].encode()

    engine = create_engine(database_url)
    async with session_factory(engine)() as session:
        if await session.scalar(select(models.Tenant).where(models.Tenant.id == TENANT)):
            print(f"already seeded; key is {PRESENTED}", file=sys.stderr)
            await engine.dispose()
            return 0

        session.add(models.Tenant(id=TENANT, tier="demo", version=1, min_trust_tier=1))
        session.add(models.KeyPrefix(prefix=PREFIX, tenant_id=TENANT))
        session.add(models.Org(id="demo-org", tenant_id=TENANT, name="Demo"))
        session.add(models.Team(id="demo-team", org_id="demo-org", name="Demo"))
        session.add(models.Application(id="demo-app", team_id="demo-team", name="Demo"))
        session.add(
            models.Budget(
                id="demo-budget",
                tenant_id=TENANT,
                scope=5,
                limit_micro_usd=50_000_000,
                spent_micro_usd=0,
                hard=True,
                headroom_basis_points=0,
            )
        )

        # Two deployments for one model, so the router has something to choose
        # between and something to fail over to. The first is unreachable on
        # purpose: a fleet where nothing ever fails exercises no failover.
        session.add(
            models.Deployment(
                id="unreachable-1",
                base_model="echo-model",
                provider="openai-compatible",
                endpoint="http://127.0.0.1:1/v1",
                trust_tier=3,
                weight=100,
            )
        )
        session.add(
            models.Deployment(
                id="echo-1",
                base_model="echo-model",
                provider="echo",
                endpoint="in-process",
                trust_tier=3,
                weight=100,
                input_cost_micro_usd=1_000,
                output_cost_micro_usd=2_000,
            )
        )

        alias = models.Alias(tenant_id=None, name="fast")
        alias.targets = [models.AliasTarget(position=0, base_model="echo-model")]
        session.add(alias)

        session.add(
            models.ApiKey(
                id="demo-key",
                tenant_id=TENANT,
                application_id="demo-app",
                lookup=compute_key_lookup(pepper, SECRET),
                models_allow_all=True,
            )
        )
        await session.commit()

    await engine.dispose()
    print(f"seeded; key is {PRESENTED}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
