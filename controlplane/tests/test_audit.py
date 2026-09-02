"""The audit chain: appending, and what a check catches afterwards.

The tests that matter here are the tampering ones. A hash chain nobody has
watched fail is a chain nobody knows works — so each way of breaking it gets
its own case, and each asserts that the report names where.
"""

from __future__ import annotations

import os
from collections.abc import AsyncIterator
from datetime import UTC, datetime, timedelta

import pytest
import pytest_asyncio
from sqlalchemy import delete, update
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.db.models import Base
from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.domain.audit import GENESIS, Link, compute_hash
from model_gateway_control.service.audit import AuditLog, Entry

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


def entry(
    event_id: str, *, action: str = "chat_completions", outcome: str = "", n: int = 0
) -> Entry:
    return Entry(
        event_id=event_id,
        action=action,
        occurred_at=BASE + timedelta(seconds=n),
        request_id=event_id.split(":")[0],
        tenant="acme",
        actor="key-1",
        resource="echo-1",
        outcome=outcome,
        snapshot_version=7,
    )


async def appended(session: AsyncSession, count: int) -> AuditLog:
    log = AuditLog(session)
    await log.append([entry(f"req-{i}:chat_completions", n=i) for i in range(count)])
    await session.commit()
    return log


# --- appending --------------------------------------------------------------


async def test_the_first_record_follows_genesis(session: AsyncSession) -> None:
    # "The chain starts here" and "the previous hash was lost" have to be
    # different values, or a truncation at the front is undetectable.
    log = await appended(session, 1)
    row = (await session.execute(models.AuditRecord.__table__.select())).first()

    assert row is not None
    assert row.prev_hash == GENESIS
    assert row.seq == 1
    assert (await log.verify()).intact


async def test_each_record_points_at_the_one_before(session: AsyncSession) -> None:
    await appended(session, 4)
    rows = list((await session.execute(models.AuditRecord.__table__.select())).all())

    hashes = [GENESIS] + [row.hash for row in rows]
    for i, row in enumerate(rows):
        assert row.prev_hash == hashes[i]
    assert len({row.hash for row in rows}) == len(rows)


async def test_a_redelivered_event_does_not_extend_the_chain(session: AsyncSession) -> None:
    # At-least-once delivery means this happens on every restart. Appending it
    # twice would put the same fact in the record twice and shift every
    # sequence number after it.
    log = AuditLog(session)
    first = await log.append([entry("req-1:chat_completions")])
    await session.commit()
    second = await log.append([entry("req-1:chat_completions")])
    await session.commit()

    assert first.appended == 1
    assert second.appended == 0
    assert second.duplicates == 1

    verdict = await log.verify()
    assert verdict.checked == 1
    assert verdict.intact


async def test_an_empty_batch_touches_nothing(session: AsyncSession) -> None:
    result = await AuditLog(session).append([])
    assert result.appended == 0


async def test_a_second_batch_continues_the_first(session: AsyncSession) -> None:
    # The head is read from the table, not held in memory, so a restarted
    # consumer picks the chain up rather than starting a second one.
    log = await appended(session, 2)
    await log.append([entry("req-9:chat_completions", n=9)])
    await session.commit()

    verdict = await AuditLog(session).verify()
    assert verdict.checked == 3
    assert verdict.intact


# --- what the chain is for --------------------------------------------------


async def test_a_deleted_record_is_detected(session: AsyncSession) -> None:
    log = await appended(session, 5)
    await session.execute(delete(models.AuditRecord).where(models.AuditRecord.seq == 3))
    await session.commit()

    verdict = await log.verify()
    assert not verdict.intact
    # Reported at the record that no longer follows its predecessor, which is
    # the one after the hole.
    assert verdict.broken_at == 4
    assert "missing or was changed" in verdict.reason


async def test_an_edited_record_is_detected(session: AsyncSession) -> None:
    # The case the chain exists for: someone changes what a refusal said.
    log = await appended(session, 3)
    await session.execute(
        update(models.AuditRecord)
        .where(models.AuditRecord.seq == 2)
        .values(outcome="", reason="looked fine to me")
    )
    await session.commit()

    verdict = await log.verify()
    assert not verdict.intact
    assert verdict.broken_at == 2
    assert "changed after it was written" in verdict.reason


async def test_an_edit_that_also_rewrites_the_hash_moves_the_break_along(
    session: AsyncSession,
) -> None:
    # A tamperer who recomputes the row's own hash defeats the content check
    # but not the link: every record after it still points at the old value.
    # This is the property that makes a single-row edit unrepairable without
    # rewriting the whole tail.
    log = await appended(session, 4)
    row = (
        await session.execute(
            models.AuditRecord.__table__.select().where(models.AuditRecord.seq == 2)
        )
    ).one()
    forged = Link(
        seq=row.seq,
        event_id=row.event_id,
        request_id=row.request_id,
        occurred_at=row.occurred_at.replace(tzinfo=UTC),
        tenant=row.tenant_id,
        actor="somebody-else",
        action=row.action,
        resource=row.resource,
        outcome=row.outcome,
        reason=row.reason,
        source_ip=row.source_ip,
        snapshot_version=row.snapshot_version,
    )
    await session.execute(
        update(models.AuditRecord)
        .where(models.AuditRecord.seq == 2)
        .values(actor="somebody-else", hash=compute_hash(forged, row.prev_hash))
    )
    await session.commit()

    verdict = await log.verify()
    assert not verdict.intact
    # Record 2 now hashes correctly; record 3 points at what 2 used to be.
    assert verdict.broken_at == 3


async def test_an_empty_chain_verifies(session: AsyncSession) -> None:
    verdict = await AuditLog(session).verify()
    assert verdict.intact
    assert verdict.checked == 0
    assert verdict.head == GENESIS


async def test_the_head_is_the_last_records_hash(session: AsyncSession) -> None:
    # The value worth publishing where the writer cannot reach: comparing it
    # against yesterday's copy catches a rewrite of the entire chain, which no
    # internal check can.
    log = await appended(session, 3)
    verdict = await log.verify()
    last = (
        await session.execute(
            models.AuditRecord.__table__.select().order_by(models.AuditRecord.seq.desc()).limit(1)
        )
    ).one()

    assert verdict.head == last.hash


def test_the_hash_covers_every_field_that_carries_meaning() -> None:
    # A field left out of the hash is a field that can be changed silently, so
    # this walks them rather than trusting the implementation to have listed
    # them all.
    base = Link(
        seq=1,
        event_id="e",
        request_id="r",
        occurred_at=BASE,
        tenant="acme",
        actor="key-1",
        action="chat_completions",
        resource="echo-1",
        outcome="",
        reason="",
        source_ip="10.0.0.1",
        snapshot_version=7,
    )
    original = compute_hash(base, GENESIS)

    changes = {
        "seq": 2,
        "event_id": "e2",
        "request_id": "r2",
        "occurred_at": BASE + timedelta(seconds=1),
        "tenant": "other",
        "actor": "key-2",
        "action": "key.issue",
        "resource": "echo-2",
        "outcome": "forbidden",
        "reason": "because",
        "source_ip": "10.0.0.2",
        "snapshot_version": 8,
    }
    for field, value in changes.items():
        altered = compute_hash(
            Link(**{**{f: getattr(base, f) for f in changes}, field: value}), GENESIS
        )
        assert altered != original, f"{field} is not covered by the hash"

    # And the predecessor, which is the link itself.
    assert compute_hash(base, "f" * 64) != original


def test_fields_cannot_be_shuffled_between_each_other() -> None:
    # The classic length-extension-by-concatenation bug: ("ab", "c") and
    # ("a", "bc") hashing alike would let a resource be moved into an actor.
    def link(actor: str, action: str) -> Link:
        return Link(
            seq=1,
            event_id="e",
            request_id="r",
            occurred_at=BASE,
            tenant="acme",
            actor=actor,
            action=action,
            resource="",
            outcome="",
            reason="",
            source_ip="",
            snapshot_version=0,
        )

    assert compute_hash(link("ab", "c"), GENESIS) != compute_hash(link("a", "bc"), GENESIS)


# --- reading ----------------------------------------------------------------


async def test_recent_returns_newest_first(session: AsyncSession) -> None:
    log = await appended(session, 5)
    rows = await log.recent(limit=3)

    assert [row.seq for row in rows] == [5, 4, 3]


async def test_refusals_can_be_isolated(session: AsyncSession) -> None:
    log = AuditLog(session)
    await log.append(
        [
            entry("a:x", outcome="", n=0),
            entry("b:x", outcome="forbidden", n=1),
            entry("c:x", outcome="", n=2),
        ]
    )
    await session.commit()

    refused = await log.recent(refusals_only=True)
    assert [row.event_id for row in refused] == ["b:x"]


async def test_records_can_be_narrowed_to_one_action(session: AsyncSession) -> None:
    log = AuditLog(session)
    await log.append([entry("a:x", action="key.issue"), entry("b:y", action="policy.publish", n=1)])
    await session.commit()

    assert [row.event_id for row in await log.recent(action="key.issue")] == ["a:x"]
    assert await log.action_summary() == {"key.issue": 1, "policy.publish": 1}


@pytest.mark.parametrize("limit", [0, -1, 10_000])
async def test_a_listing_is_always_bounded(session: AsyncSession, limit: int) -> None:
    await appended(session, 3)
    rows = await AuditLog(session).recent(limit=limit)
    assert 1 <= len(rows) <= 3
