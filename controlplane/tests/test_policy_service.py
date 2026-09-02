"""Publishing policy: the seam an external authority writes rules through."""

from __future__ import annotations

import os
from collections.abc import AsyncIterator

import pytest
import pytest_asyncio
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.db.models import Base
from model_gateway_control.db.repository import Repository
from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.domain.policy import PolicyEffect, PolicyRule
from model_gateway_control.errors import InvalidRequestError, NotFoundError
from model_gateway_control.service.policy import PolicyService

DEFAULT_URL = "sqlite+aiosqlite:///:memory:"


@pytest_asyncio.fixture
async def session() -> AsyncIterator[AsyncSession]:
    engine = create_engine(os.environ.get("GATEWAY_TEST_DATABASE_URL", DEFAULT_URL))
    async with engine.begin() as connection:
        await connection.run_sync(Base.metadata.drop_all)
        await connection.run_sync(Base.metadata.create_all)
    async with session_factory(engine)() as session:
        # create_all rather than migrations, so the row the initial migration
        # seeds has to be added by hand.
        session.add(models.FleetState(id=1, version=1, policy_bundle_ref=""))
        session.add(models.Tenant(id="acme", tier="demo", version=1, min_trust_tier=1))
        await session.flush()
        yield session
    await engine.dispose()


def rule(rule_id: str, *, allow: bool = True, models_: tuple[str, ...] = ()) -> PolicyRule:
    return PolicyRule(
        id=rule_id,
        effect=PolicyEffect.ALLOW if allow else PolicyEffect.DENY,
        models=models_,
        reason="" if allow else "not approved",
    )


async def test_conditions_survive_the_round_trip(session: AsyncSession) -> None:
    # The bug this exists to prevent: the writer stored "models" and the reader
    # looked for "model", so a rule naming a model was stored and then silently
    # read back with no model condition — which matches *every* model. A policy
    # that names one model and applies to all of them is worse than no policy,
    # because it looks like it is working.
    service = PolicyService(session)

    await service.replace(None, [rule("only-qwen", models_=("qwen2.5:0.5b",))])
    stored = await service.get(None)

    assert stored.rules[0].models == ("qwen2.5:0.5b",)


async def test_the_snapshot_loader_reads_what_the_writer_wrote(
    session: AsyncSession,
) -> None:
    # The reader that matters is the one compiling the snapshot, not the one on
    # the API. Asserting through the repository is what proves they agree.
    await PolicyService(session).replace(None, [rule("only-qwen", models_=("qwen2.5:0.5b",))])
    await session.flush()

    fleet = await Repository(session).load_fleet()

    assert fleet.default_policy is not None
    assert fleet.default_policy.rules[0].models == ("qwen2.5:0.5b",)


async def test_publishing_replaces_rather_than_appends(session: AsyncSession) -> None:
    # An authority restating its position should not have to work out what it
    # said last time, and a retry after a crash must not double the rule set.
    service = PolicyService(session)

    await service.replace(None, [rule("first"), rule("second")])
    await service.replace(None, [rule("only-this-one")])

    stored = await service.get(None)
    assert [r.id for r in stored.rules] == ["only-this-one"]


async def test_publishing_the_same_rules_twice_is_a_no_op(session: AsyncSession) -> None:
    service = PolicyService(session)
    rules = [rule("a"), rule("b", allow=False)]

    await service.replace(None, rules)
    await service.replace(None, rules)

    assert [r.id for r in (await service.get(None)).rules] == ["a", "b"]


async def test_order_is_preserved_because_first_match_wins(session: AsyncSession) -> None:
    # Position is the whole of the conflict resolution. A set would lose it.
    service = PolicyService(session)

    await service.replace(None, [rule("deny-first", allow=False), rule("allow-rest")])

    assert [r.id for r in (await service.get(None)).rules] == ["deny-first", "allow-rest"]


async def test_a_rejected_rule_set_leaves_the_previous_one_in_place(
    session: AsyncSession,
) -> None:
    # A publish that half-applies is worse than one that fails: the operator
    # believes the new policy is in force and some of the old one is.
    service = PolicyService(session)
    await service.replace(None, [rule("original")])

    with pytest.raises(InvalidRequestError, match="duplicate policy rule"):
        await service.replace(None, [rule("duplicate"), rule("duplicate")])

    assert [r.id for r in (await service.get(None)).rules] == ["original"]


async def test_a_tenant_policy_is_separate_from_the_fleet_default(
    session: AsyncSession,
) -> None:
    service = PolicyService(session)

    await service.replace(None, [rule("fleet-rule")])
    await service.replace("acme", [rule("tenant-rule")])

    assert [r.id for r in (await service.get(None)).rules] == ["fleet-rule"]
    assert [r.id for r in (await service.get("acme")).rules] == ["tenant-rule"]


async def test_publishing_for_an_unknown_tenant_is_refused(session: AsyncSession) -> None:
    with pytest.raises(NotFoundError, match="no tenant"):
        await PolicyService(session).replace("nobody", [rule("a")])


async def test_an_empty_rule_set_withdraws_every_rule(session: AsyncSession) -> None:
    service = PolicyService(session)
    await service.replace(None, [rule("a")])

    await service.replace(None, [])

    withdrawn = await service.get(None)
    assert withdrawn.rules == ()
