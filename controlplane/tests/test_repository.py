"""The repository reads the source of truth into the domain model.

These run against whatever ``GATEWAY_TEST_DATABASE_URL`` names, defaulting to
in-memory SQLite for fast local feedback. CI points it at Postgres and that run
is the gate.

Testing only on SQLite while running on Postgres is a well-known trap. The
mitigation is that this is the *same* suite against both, and the schema is
deliberately free of engine-specific types so it stays expressible in each.
"""

from __future__ import annotations

import os
from collections.abc import AsyncIterator
from datetime import UTC, datetime

import pytest
import pytest_asyncio
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.db.models import Base
from model_gateway_control.db.repository import Repository
from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.domain.budget import BudgetScope
from model_gateway_control.domain.catalog import Capability, TrustTier
from model_gateway_control.errors import NotFoundError
from model_gateway_control.snapshot import build_snapshot

DEFAULT_URL = "sqlite+aiosqlite:///:memory:"


@pytest_asyncio.fixture
async def session() -> AsyncIterator[AsyncSession]:
    """A session against a schema built fresh for this test.

    The schema is dropped and recreated per test rather than merely created.
    In-memory SQLite gives each connection its own empty database, so creating
    was enough locally — but Postgres persists across tests, and the second one
    to call ``seed`` collided on a primary key. That divergence is exactly why
    the Postgres run is the gate rather than a formality.

    ``create_all`` is used rather than migrations because these tests are about
    the mapping. That the migration produces the same shape is checked in
    test_migrations.py.
    """
    engine = create_engine(os.environ.get("GATEWAY_TEST_DATABASE_URL", DEFAULT_URL))
    async with engine.begin() as connection:
        await connection.run_sync(Base.metadata.drop_all)
        await connection.run_sync(Base.metadata.create_all)

    factory = session_factory(engine)
    async with factory() as session:
        yield session
    await engine.dispose()


async def seed(session: AsyncSession) -> None:
    """A small but complete fleet: two keys, one via an app and one via a user."""
    session.add(models.FleetState(id=1, version=9, policy_bundle_ref="bundle-9"))
    session.add(models.Tenant(id="acme", tier="enterprise", version=4, min_trust_tier=1))
    session.add(models.KeyPrefix(prefix="acme", tenant_id="acme"))
    session.add(models.Org(id="acme-org", tenant_id="acme", name="Acme"))
    session.add(models.Team(id="platform", org_id="acme-org", name="Platform"))
    session.add(models.Application(id="app-1", team_id="platform", name="Triage bot"))
    session.add(models.User(id="user-1", team_id="platform", subject="ada@acme.test"))

    session.add(
        models.Budget(
            id="monthly",
            tenant_id="acme",
            scope=int(BudgetScope.ORG),
            limit_micro_usd=5_000_000,
            spent_micro_usd=1_250_000,
        )
    )

    session.add(
        models.Deployment(
            id="openai-1",
            base_model="gpt-4o-mini",
            provider="openai-compatible",
            endpoint="https://api.openai.com/v1",
            trust_tier=int(TrustTier.EXTERNAL),
            credential_ref="env:OPENAI_API_KEY",
            weight=100,
            input_cost_micro_usd=150,
            output_cost_micro_usd=600,
        )
    )
    session.add(
        models.DeploymentCapability(deployment_id="openai-1", capability=str(Capability.STREAMING))
    )
    # A fine-tuned adapter on the same base model: a distinct routing key, and
    # weight 0 because it has not been promoted.
    session.add(
        models.Deployment(
            id="openai-1-triage",
            base_model="gpt-4o-mini",
            adapter_id="triage-v3",
            provider="openai-compatible",
            endpoint="https://api.openai.com/v1",
            trust_tier=int(TrustTier.EXTERNAL),
            weight=0,
        )
    )

    global_alias = models.Alias(tenant_id=None, name="fast")
    global_alias.targets = [models.AliasTarget(position=0, base_model="gpt-4o-mini")]
    session.add(global_alias)

    tenant_alias = models.Alias(tenant_id="acme", name="fast")
    tenant_alias.targets = [
        models.AliasTarget(position=0, base_model="gpt-4o-mini", adapter_id="triage-v3")
    ]
    session.add(tenant_alias)

    session.add(models.PluginBinding(tenant_id=None, port="guardrail", component="regex-pii"))
    session.add(models.PluginBinding(tenant_id="acme", port="guardrail", component="presidio"))

    app_key = models.ApiKey(
        id="key-app",
        tenant_id="acme",
        application_id="app-1",
        lookup=b"\x01" * 32,
        models_allow_all=True,
        default_data_class="confidential",
        min_trust_tier=int(TrustTier.EXTERNAL),
        max_concurrent=32,
    )
    app_key.roles = [models.KeyRole(role="admin"), models.KeyRole(role="billing")]
    app_key.budgets = [models.KeyBudget(budget_id="monthly")]
    session.add(app_key)

    user_key = models.ApiKey(id="key-user", tenant_id="acme", user_id="user-1", lookup=b"\x02" * 32)
    user_key.models = [models.KeyModel(model="fast")]
    session.add(user_key)

    # Revoked: kept for the audit trail, excluded from the snapshot.
    session.add(
        models.ApiKey(
            id="key-revoked",
            tenant_id="acme",
            application_id="app-1",
            lookup=b"\x03" * 32,
            revoked_at=datetime.now(UTC),
        )
    )
    await session.commit()


async def test_load_fleet(session: AsyncSession) -> None:
    await seed(session)
    fleet = await Repository(session).load_fleet()

    assert fleet.version == 9
    assert fleet.policy_bundle_ref == "bundle-9"
    assert len(fleet.deployments) == 2
    # Only the fleet-wide alias and plugin, not the tenant's overrides.
    assert [a.name for a in fleet.aliases] == ["fast"]
    assert [p.component for p in fleet.default_plugins] == ["regex-pii"]


async def test_deployment_mapping(session: AsyncSession) -> None:
    await seed(session)
    fleet = await Repository(session).load_fleet()
    by_id = {d.id: d for d in fleet.deployments}

    base = by_id["openai-1"]
    assert base.key.base_model == "gpt-4o-mini"
    assert base.key.adapter_id == ""
    assert base.trust_tier is TrustTier.EXTERNAL
    assert base.cost.input_per_1k_micro_usd == 150
    assert base.capabilities == (Capability.STREAMING,)
    # The credential is a reference. Nothing secret-shaped is stored.
    assert base.credential_ref == "env:OPENAI_API_KEY"

    adapter = by_id["openai-1-triage"]
    assert adapter.key.adapter_id == "triage-v3"
    assert adapter.weight == 0
    assert base.key != adapter.key


async def test_ancestry_is_flattened_into_the_principal(session: AsyncSession) -> None:
    # This is the part of the plan's §5.1 that matters: the graph stops being a
    # graph here, once per build, so admission is a hash lookup.
    await seed(session)
    tenant = await Repository(session).load_tenant("acme")
    by_id = {p.key_id: p for p in tenant.principals}

    via_app = by_id["key-app"]
    assert (via_app.org, via_app.team, via_app.app, via_app.user) == (
        "acme-org",
        "platform",
        "app-1",
        "",
    )

    via_user = by_id["key-user"]
    assert (via_user.org, via_user.team, via_user.user, via_user.app) == (
        "acme-org",
        "platform",
        "user-1",
        "",
    )


async def test_revoked_keys_are_excluded_but_not_deleted(session: AsyncSession) -> None:
    await seed(session)
    tenant = await Repository(session).load_tenant("acme")

    assert {p.key_id for p in tenant.principals} == {"key-app", "key-user"}
    assert b"\x03" * 32 not in tenant.keys
    # Still on record, which is the point of revoking rather than deleting.
    revoked = await session.get(models.ApiKey, "key-revoked")
    assert revoked is not None
    assert revoked.revoked_at is not None


async def test_allowlist_fails_closed(session: AsyncSession) -> None:
    # A key with no allowlist rows and no allow_all permits nothing.
    await seed(session)
    tenant = await Repository(session).load_tenant("acme")
    by_id = {p.key_id: p for p in tenant.principals}

    assert by_id["key-app"].models_allow_all is True
    assert by_id["key-user"].models_allow_all is False
    assert by_id["key-user"].models == ("fast",)


async def test_tenant_overrides_are_kept_separate(session: AsyncSession) -> None:
    await seed(session)
    repo = Repository(session)
    fleet = await repo.load_fleet()
    tenant = await repo.load_tenant("acme")

    assert fleet.aliases[0].targets[0].adapter_id == ""
    assert tenant.alias_overrides[0].targets[0].adapter_id == "triage-v3"
    assert [p.component for p in tenant.plugins] == ["presidio"]


async def test_budget_chain_and_scope(session: AsyncSession) -> None:
    await seed(session)
    tenant = await Repository(session).load_tenant("acme")
    by_id = {p.key_id: p for p in tenant.principals}

    assert [b.id for b in by_id["key-app"].budgets] == ["monthly"]
    assert by_id["key-app"].budgets[0].scope is BudgetScope.ORG
    assert tenant.budgets[0].available_micro_usd == 5_000_000 - 250_000 - 1_250_000


async def test_an_unknown_tenant_is_an_error(session: AsyncSession) -> None:
    await seed(session)
    with pytest.raises(NotFoundError, match="no tenant"):
        await Repository(session).load_tenant("nobody")


async def test_uninitialised_fleet_state_is_an_error(session: AsyncSession) -> None:
    # Seeding row 1 is the migration's job, so a missing row is a real fault
    # rather than a normal first-run state to paper over.
    with pytest.raises(NotFoundError, match="fleet state"):
        await Repository(session).load_fleet()


async def test_what_the_repository_loads_builds_a_valid_snapshot(session: AsyncSession) -> None:
    # The repository and the builder are separately correct only if what one
    # produces is what the other accepts. This is the seam between them.
    await seed(session)
    repo = Repository(session)

    snapshot = build_snapshot(await repo.load_fleet(), await repo.load_tenants(), datetime.now(UTC))

    assert snapshot.global_layer.version.number == 9
    assert snapshot.global_layer.version.digest.startswith("sha256:")
    assert dict(snapshot.global_layer.tenant_prefixes) == {"acme": "acme"}
    assert len(snapshot.tenants) == 1
    assert len(snapshot.tenants[0].principals) == 2
    assert len(snapshot.tenants[0].keys) == 2


async def test_two_builds_of_the_same_database_agree(session: AsyncSession) -> None:
    # Row ordering from the database must not reach the digest, or the same
    # configuration produces a different artifact on every build and workers
    # re-fetch a snapshot they already hold.
    await seed(session)
    repo = Repository(session)
    at = datetime(2026, 9, 1, tzinfo=UTC)

    first = build_snapshot(await repo.load_fleet(), await repo.load_tenants(), at)
    second = build_snapshot(await repo.load_fleet(), await repo.load_tenants(), at)

    assert first.global_layer.version.digest == second.global_layer.version.digest
    assert first.tenants[0].version.digest == second.tenants[0].version.digest
