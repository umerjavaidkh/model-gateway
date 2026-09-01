"""The accounting consumer closes the budget loop.

    data plane -> usage event -> stream -> accountant -> budget spend
                                                            |
                       admission refuses an exhausted budget +

These test the accountant against a database. The stream reader is exercised
separately, because the interesting property here is idempotency and that lives
entirely in what gets written.
"""

from __future__ import annotations

import os
from collections.abc import AsyncIterator
from datetime import UTC, datetime

import pytest_asyncio
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.db.models import Base
from model_gateway_control.db.repository import Repository
from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.domain.budget import BudgetScope
from model_gateway_control.service.accounting import Accountant
from model_gateway_control.wire import usage_pb2 as pb

DEFAULT_URL = "sqlite+aiosqlite:///:memory:"


@pytest_asyncio.fixture
async def session() -> AsyncIterator[AsyncSession]:
    engine = create_engine(os.environ.get("GATEWAY_TEST_DATABASE_URL", DEFAULT_URL))
    async with engine.begin() as connection:
        await connection.run_sync(Base.metadata.drop_all)
        await connection.run_sync(Base.metadata.create_all)

    factory = session_factory(engine)
    async with factory() as session:
        session.add(models.FleetState(id=1, version=1, policy_bundle_ref=""))
        session.add(models.Tenant(id="acme", tier="enterprise", version=1, min_trust_tier=1))
        session.add(models.KeyPrefix(prefix="acme", tenant_id="acme"))
        session.add(
            models.Budget(
                id="monthly",
                tenant_id="acme",
                scope=int(BudgetScope.ORG),
                limit_micro_usd=1_000_000,
                spent_micro_usd=0,
            )
        )
        await session.commit()
        yield session
    await engine.dispose()


def event(
    request_id: str, *, price: int = 1000, budgets: tuple[str, ...] = ("monthly",)
) -> pb.UsageEvent:
    return pb.UsageEvent(
        request_id=request_id,
        timestamp_unix_ms=int(datetime.now(UTC).timestamp() * 1000),
        tenant="acme",
        key_id="key-1",
        usage=pb.TokenUsage(input=100, cached_input=900, output=50),
        cost_micro_usd=price,
        price_micro_usd=price,
        budget_ids=list(budgets),
    )


async def test_spend_is_charged_to_the_named_budgets(session: AsyncSession) -> None:
    accountant = Accountant(session)
    await accountant.apply([event("r1"), event("r2")])
    await session.commit()

    assert await accountant.spend_for("monthly") == 2000
    assert await accountant.record_count() == 2


async def test_replaying_an_event_does_not_charge_twice(session: AsyncSession) -> None:
    # The stream is at-least-once, so a restart or a redelivery shows the same
    # event again. Charging it twice overcharges, and a running total gives no
    # way to detect that afterwards.
    accountant = Accountant(session)

    first = await accountant.apply([event("r1")])
    second = await accountant.apply([event("r1")])
    await session.commit()

    assert first.applied == 1
    assert second.applied == 0
    assert second.duplicates == 1
    assert await accountant.spend_for("monthly") == 1000


async def test_a_duplicate_inside_one_batch_is_caught(session: AsyncSession) -> None:
    # Redelivery can put the same event twice in one read, not only across
    # restarts.
    accountant = Accountant(session)
    await accountant.apply([event("r1"), event("r1"), event("r2")])
    await session.commit()

    assert await accountant.spend_for("monthly") == 2000


async def test_an_event_with_no_id_is_not_counted(session: AsyncSession) -> None:
    # Without an id there is no way to deduplicate it, so counting it would
    # make every redelivery an overcharge.
    accountant = Accountant(session)
    result = await accountant.apply([event("")])
    await session.commit()

    assert result.applied == 0
    assert await accountant.spend_for("monthly") == 0


async def test_a_deleted_budget_does_not_wedge_the_consumer(session: AsyncSession) -> None:
    # A budget removed after a request was served must not leave a message the
    # consumer can never process, or everything behind it stops.
    accountant = Accountant(session)
    result = await accountant.apply([event("r1", budgets=("gone",))])
    await session.commit()

    assert result.applied == 1
    assert result.unknown_budgets == 1


async def test_a_refused_request_records_usage_without_spend(session: AsyncSession) -> None:
    # A request rejected at admission still happened and is worth recording,
    # but it cost nothing and must not move a budget.
    accountant = Accountant(session)
    await accountant.apply([event("r1", price=0)])
    await session.commit()

    assert await accountant.record_count() == 1
    assert await accountant.spend_for("monthly") == 0


async def test_charging_advances_the_tenant_version(session: AsyncSession) -> None:
    # A worker rejects a layer whose version has not moved forward, so spend
    # recorded without this never reaches the data plane and the budget never
    # actually denies anything.
    before = (await session.get(models.Tenant, "acme")).version  # type: ignore[union-attr]

    await Accountant(session).apply([event("r1")])
    await session.commit()

    after = (await session.get(models.Tenant, "acme")).version  # type: ignore[union-attr]
    assert after > before


async def test_the_version_moves_once_per_batch_not_once_per_event(
    session: AsyncSession,
) -> None:
    # Every bump is a snapshot the whole fleet re-fetches. One per request would
    # make propagation cost scale with traffic rather than with change.
    before = (await session.get(models.Tenant, "acme")).version  # type: ignore[union-attr]

    await Accountant(session).apply([event(f"r{i}") for i in range(20)])
    await session.commit()

    after = (await session.get(models.Tenant, "acme")).version  # type: ignore[union-attr]
    assert after == before + 1


async def test_recorded_spend_reaches_the_next_snapshot(session: AsyncSession) -> None:
    # The loop, end to end on this side: what the consumer writes is what the
    # builder reads and the worker will enforce.
    await Accountant(session).apply([event("r1", price=750_000)])
    await session.commit()

    tenant = await Repository(session).load_tenant("acme")
    budget = tenant.budgets[0]

    assert budget.spent_micro_usd == 750_000
    # 1,000,000 limit less 5% headroom less 750,000 spent.
    assert budget.available_micro_usd == 200_000


async def test_an_exhausted_budget_reports_nothing_available(session: AsyncSession) -> None:
    await Accountant(session).apply([event("r1", price=1_000_000)])
    await session.commit()

    tenant = await Repository(session).load_tenant("acme")
    assert tenant.budgets[0].available_micro_usd == 0
