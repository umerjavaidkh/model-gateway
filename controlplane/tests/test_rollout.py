"""Automatic promotion: when a canary has earned its next step.

Three outcomes, not two. "Advance", "abort" and "wait" are different, and
collapsing the last two either aborts healthy rollouts that are merely quiet or
advances ones nothing has measured — both of which are worse than the manual
walk this replaces.
"""

from __future__ import annotations

import os
from collections.abc import AsyncIterator
from datetime import UTC, datetime, timedelta

import pytest
import pytest_asyncio
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.db.models import Base
from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.errors import InvalidRequestError
from model_gateway_control.service.rollout import Observed, Policy, RolloutHealth

DEFAULT_URL = "sqlite+aiosqlite:///:memory:"
STARTED = datetime(2026, 9, 2, 12, 0, tzinfo=UTC)


@pytest_asyncio.fixture
async def session() -> AsyncIterator[AsyncSession]:
    engine = create_engine(os.environ.get("GATEWAY_TEST_DATABASE_URL", DEFAULT_URL))
    async with engine.begin() as connection:
        await connection.run_sync(Base.metadata.drop_all)
        await connection.run_sync(Base.metadata.create_all)
    async with session_factory(engine)() as session:
        yield session
    await engine.dispose()


async def record(
    session: AsyncSession, deployment: str, *, n: int, errors: int, shadow: bool = False
) -> None:
    """Write n usage records for a deployment, of which `errors` failed."""
    for i in range(n):
        session.add(
            models.UsageRecord(
                request_id=f"{deployment}-{shadow}-{i}",
                tenant_id="acme",
                key_id="key-1",
                occurred_at=STARTED + timedelta(seconds=i),
                deployment=deployment,
                shadow=shadow,
                outcome="upstream_error" if i < errors else "",
            )
        )
    await session.flush()


def judged_at(minutes: int) -> datetime:
    return STARTED + timedelta(minutes=minutes)


# --- the rate ---------------------------------------------------------------


def test_an_error_rate_with_no_requests_is_zero_not_undefined() -> None:
    # Dividing by the request count is the obvious implementation and it
    # crashes on the case that happens most: a canary nobody has hit yet.
    assert Observed(requests=0, errors=0).error_rate == 0


def test_the_rate_is_basis_points() -> None:
    # Integers, like every other rate here: a threshold decided by the last
    # bits of a float can go differently on two machines.
    assert Observed(requests=1000, errors=25).error_rate == 250


# --- the three outcomes -----------------------------------------------------


async def test_a_step_that_has_not_run_long_enough_waits(session: AsyncSession) -> None:
    # A step judged the instant it starts is judged on nothing.
    await record(session, "acme-triage", n=1000, errors=0)
    health = RolloutHealth(session, Policy(dwell=timedelta(minutes=15)))

    verdict = await health.judge("acme-triage", "llama", STARTED, judged_at(5))

    assert verdict.wait
    assert "dwell" in verdict.reason


async def test_a_step_with_too_little_traffic_waits(session: AsyncSession) -> None:
    # A canary at 1% of a quiet tenant's traffic may never reach the floor, and
    # that is the correct outcome: it waits, and an operator advances it by
    # hand knowing they are doing so without evidence.
    await record(session, "acme-triage", n=3, errors=0)
    health = RolloutHealth(session, Policy(min_requests=30))

    verdict = await health.judge("acme-triage", "llama", STARTED, judged_at(60))

    assert verdict.wait
    assert "not enough to judge" in verdict.reason


async def test_a_healthy_canary_advances(session: AsyncSession) -> None:
    await record(session, "acme-triage", n=100, errors=1)
    await record(session, "llama", n=1000, errors=10)
    health = RolloutHealth(session)

    verdict = await health.judge("acme-triage", "llama", STARTED, judged_at(60))

    assert verdict.advance
    assert not verdict.abort


async def test_a_canary_failing_worse_than_the_base_model_aborts(
    session: AsyncSession,
) -> None:
    # The failure automatic promotion exists to catch: an adapter that passed
    # its eval suite and then falls over on real traffic.
    await record(session, "acme-triage", n=100, errors=40)
    await record(session, "llama", n=1000, errors=10)
    health = RolloutHealth(session)

    verdict = await health.judge("acme-triage", "llama", STARTED, judged_at(60))

    assert verdict.abort
    assert not verdict.advance
    assert "over the" in verdict.reason


async def test_a_canary_matching_a_bad_base_model_still_advances(
    session: AsyncSession,
) -> None:
    # The comparison is against the base model, not against perfection. A
    # provider having a bad hour must not abort every rollout riding on it.
    await record(session, "acme-triage", n=100, errors=30)
    await record(session, "llama", n=1000, errors=300)
    health = RolloutHealth(session)

    verdict = await health.judge("acme-triage", "llama", STARTED, judged_at(60))

    assert verdict.advance


async def test_a_small_excess_is_tolerated(session: AsyncSession) -> None:
    # Real traffic has a noise floor. A gate that aborts on noise is one an
    # operator stops using, and an unused gate protects nothing.
    await record(session, "acme-triage", n=1000, errors=11)
    await record(session, "llama", n=1000, errors=10)
    health = RolloutHealth(session, Policy(error_tolerance=200))

    verdict = await health.judge("acme-triage", "llama", STARTED, judged_at(60))

    assert verdict.advance


async def test_shadow_and_real_traffic_are_counted_together(session: AsyncSession) -> None:
    # An adapter's errors are its errors whether or not anybody was waiting for
    # the answer. Separating them would mean a canary's first steps were judged
    # on nothing while its whole shadow window was ignored.
    await record(session, "acme-triage", n=50, errors=0, shadow=True)
    await record(session, "llama", n=1000, errors=0)
    health = RolloutHealth(session, Policy(min_requests=30))

    verdict = await health.judge("acme-triage", "llama", STARTED, judged_at(60))

    assert verdict.advance, verdict.reason


async def test_traffic_from_before_the_step_is_not_counted(session: AsyncSession) -> None:
    # A step is judged on what happened during it. Counting an earlier step's
    # traffic would let a good first step carry a bad second one.
    session.add(
        models.UsageRecord(
            request_id="old",
            tenant_id="acme",
            key_id="key-1",
            occurred_at=STARTED - timedelta(hours=1),
            deployment="acme-triage",
            outcome="",
        )
    )
    await record(session, "acme-triage", n=3, errors=0)
    health = RolloutHealth(session, Policy(min_requests=30))

    verdict = await health.judge("acme-triage", "llama", STARTED, judged_at(60))

    assert verdict.wait
    assert "3 requests" in verdict.reason


def test_a_policy_with_no_evidence_floor_is_refused() -> None:
    with pytest.raises(InvalidRequestError, match="minimum evidence"):
        Policy(min_requests=0)
    with pytest.raises(InvalidRequestError, match="cannot be negative"):
        Policy(error_tolerance=-1)
