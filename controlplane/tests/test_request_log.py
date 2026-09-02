"""Reading what requests did — the queries a dashboard is built on."""

from __future__ import annotations

import json
import os
from collections.abc import AsyncIterator
from datetime import UTC, datetime, timedelta

import pytest
import pytest_asyncio
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.db.models import Base
from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.errors import NotFoundError
from model_gateway_control.service.requests import MAX_LIMIT, RequestLog

DEFAULT_URL = "sqlite+aiosqlite:///:memory:"
BASE = datetime(2026, 9, 2, 12, 0, tzinfo=UTC)


@pytest_asyncio.fixture
async def session() -> AsyncIterator[AsyncSession]:
    engine = create_engine(os.environ.get("GATEWAY_TEST_DATABASE_URL", DEFAULT_URL))
    async with engine.begin() as connection:
        await connection.run_sync(Base.metadata.drop_all)
        await connection.run_sync(Base.metadata.create_all)
    async with session_factory(engine)() as session:
        yield session
    await engine.dispose()


def record(
    request_id: str,
    *,
    outcome: str = "",
    seconds: int = 0,
    shadow: bool = False,
    tenant: str = "acme",
    stages: list[dict[str, object]] | None = None,
) -> models.UsageRecord:
    return models.UsageRecord(
        request_id=request_id,
        tenant_id=tenant,
        key_id="key-1",
        occurred_at=BASE + timedelta(seconds=seconds),
        outcome=outcome,
        shadow=shadow,
        deployment="qwen-1",
        base_model="qwen2.5:0.5b",
        provider="openai-compatible",
        latency_ms=120,
        input_tokens=30,
        output_tokens=8,
        cost_micro_usd=4,
        stages=json.dumps(stages if stages is not None else []),
    )


async def test_the_newest_requests_come_first(session: AsyncSession) -> None:
    # A dashboard showing the oldest hundred is showing the wrong hundred.
    session.add_all([record("old", seconds=0), record("new", seconds=60)])
    await session.flush()

    recent = await RequestLog(session).recent()

    assert [r.request_id for r in recent] == ["new", "old"]


async def test_failures_can_be_asked_for_alone(session: AsyncSession) -> None:
    session.add_all([record("ok-1"), record("bad-1", outcome="upstream_error")])
    await session.flush()

    failed = await RequestLog(session).recent(failed_only=True)

    assert [r.request_id for r in failed] == ["bad-1"]
    assert failed[0].failed


async def test_shadow_traffic_is_excluded_unless_asked_for(session: AsyncSession) -> None:
    # Mirrored requests nobody was waiting for would crowd out the ones
    # somebody was, and their failures are a finding about an adapter rather
    # than an incident.
    session.add_all([record("real"), record("mirrored", shadow=True)])
    await session.flush()

    log = RequestLog(session)

    assert [r.request_id for r in await log.recent()] == ["real"]
    assert {r.request_id for r in await log.recent(include_shadow=True)} == {"real", "mirrored"}


async def test_a_record_says_which_stage_ended_it(session: AsyncSession) -> None:
    # The question somebody actually has when a request failed. The final code
    # says what went wrong; this says where, which decides who looks at it next.
    session.add(
        record(
            "denied",
            outcome="forbidden",
            stages=[
                {"name": "authenticate", "duration_ms": 0, "outcome": ""},
                {"name": "admit", "duration_ms": 1, "outcome": "forbidden"},
            ],
        )
    )
    await session.flush()

    [denied] = await RequestLog(session).recent(failed_only=True)

    assert denied.failed_at == "admit"
    assert [s.name for s in denied.stages] == ["authenticate", "admit"]


async def test_a_successful_request_names_no_failing_stage(session: AsyncSession) -> None:
    session.add(record("fine", stages=[{"name": "adapt", "duration_ms": 120, "outcome": ""}]))
    await session.flush()

    [fine] = await RequestLog(session).recent()

    assert fine.failed_at == ""
    assert not fine.failed


async def test_a_record_written_before_stages_existed_still_reads(
    session: AsyncSession,
) -> None:
    # The column has a default of '[]', but a row written by an older consumer
    # could hold an empty string. Reading history must not raise.
    row = record("ancient")
    row.stages = ""
    session.add(row)
    await session.flush()

    [ancient] = await RequestLog(session).recent()

    assert ancient.stages == ()
    assert ancient.failed_at == ""


async def test_the_limit_is_bounded(session: AsyncSession) -> None:
    # A dashboard that can ask for a million rows will, eventually, from a tab
    # somebody left open.
    session.add_all([record(f"r{i}", seconds=i) for i in range(5)])
    await session.flush()

    assert len(await RequestLog(session).recent(limit=2)) == 2
    assert len(await RequestLog(session).recent(limit=10_000)) == 5
    assert len(await RequestLog(session).recent(limit=0)) == 1
    assert MAX_LIMIT < 10_000


async def test_failures_are_summarised_by_kind(session: AsyncSession) -> None:
    session.add_all(
        [
            record("a", outcome="upstream_error"),
            record("b", outcome="upstream_error"),
            record("c", outcome="forbidden"),
            record("d"),
        ]
    )
    await session.flush()

    summary = await RequestLog(session).failure_summary()

    # Most common first: the question is what is going wrong now.
    assert list(summary.items()) == [("upstream_error", 2), ("forbidden", 1)]


async def test_one_request_can_be_fetched_by_id(session: AsyncSession) -> None:
    session.add(record("wanted"))
    await session.flush()

    assert (await RequestLog(session).get("wanted")).request_id == "wanted"

    with pytest.raises(NotFoundError, match="no request"):
        await RequestLog(session).get("never-happened")


async def test_requests_can_be_narrowed_to_one_tenant(session: AsyncSession) -> None:
    session.add_all([record("ours", tenant="acme"), record("theirs", tenant="other")])
    await session.flush()

    assert [r.request_id for r in await RequestLog(session).recent(tenant="acme")] == ["ours"]
