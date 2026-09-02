"""Fold usage back into budgets.

# The loop this closes

    data plane -> usage event -> stream -> here -> budget spend -> snapshot
                                                                     |
                          admission refuses an exhausted budget <----+

Budgets are eventually consistent by design. This is where the lag lives, and
it is why rate limits exist separately for anything that must be immediate.

# Idempotency is the whole problem

The stream is at-least-once, so a restart or a redelivery shows the same event
again. Applying spend twice would overcharge, and there is no way to detect it
afterwards from a running total.

So the request id is a primary key. An insert that conflicts is discarded, and
spend is applied only for rows that were actually inserted. The consumer can
then be killed at any point — including between writing a record and
acknowledging it — and re-reading loses nothing and double-counts nothing.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import UTC, datetime

from sqlalchemy import select, update
from sqlalchemy.dialects.postgresql import insert as postgres_insert
from sqlalchemy.dialects.sqlite import insert as sqlite_insert
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.wire import usage_pb2 as pb


@dataclass(slots=True)
class ApplyResult:
    """What a batch did, for logging and for tests."""

    applied: int = 0
    #: Events already recorded. A steady non-zero count is normal after a
    #: restart and suspicious otherwise.
    duplicates: int = 0
    #: Events naming a budget that no longer exists. Counted rather than
    #: failed: a deleted budget must not wedge the consumer forever.
    unknown_budgets: int = 0
    tenants_touched: set[str] = field(default_factory=set)


class Accountant:
    """Applies usage events to budgets.

    Takes a session rather than creating one, so the caller owns the
    transaction boundary — which matters here, because recording a batch and
    advancing the tenant version have to land together.
    """

    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    async def apply(self, events: list[pb.UsageEvent]) -> ApplyResult:
        """Record a batch and charge its spend."""
        result = ApplyResult()
        for event in events:
            if await self._record(event, result):
                await self._charge(event, result)
        await self._bump_versions(result.tenants_touched)
        return result

    async def _record(self, event: pb.UsageEvent, result: ApplyResult) -> bool:
        """Insert the usage record, reporting whether it was new."""
        if not event.request_id:
            # Without an id there is no way to deduplicate it, so counting it
            # would make every redelivery an overcharge.
            result.duplicates += 1
            return False

        values = {
            "request_id": event.request_id,
            "tenant_id": event.tenant,
            "key_id": event.key_id,
            "occurred_at": _from_unix_ms(event.timestamp_unix_ms),
            "input_tokens": event.usage.input,
            "cached_input_tokens": event.usage.cached_input,
            "cache_write_tokens": event.usage.cache_write,
            "output_tokens": event.usage.output,
            "cost_micro_usd": event.cost_micro_usd,
            "price_micro_usd": event.price_micro_usd,
            "outcome": event.outcome,
            "deployment": event.deployment,
            "shadow": event.shadow,
            "base_model": event.base_model,
            "adapter_id": event.adapter_id,
            "provider": event.provider,
            "stream": event.stream,
            "latency_ms": event.latency_ms,
            "time_to_first_byte_ms": event.time_to_first_byte_ms,
            "snapshot_version": event.snapshot_version,
            # In the order they ran, which is what makes the list readable.
            "stages": json.dumps(
                [
                    {"name": s.name, "duration_ms": s.duration_ms, "outcome": s.outcome}
                    for s in event.stages
                ]
            ),
        }

        statement = (
            _upsert(self._session)
            .values(**values)
            .on_conflict_do_nothing(index_elements=["request_id"])
        )
        # rowcount is 0 when the conflict clause discarded the row, which is
        # exactly the duplicate case. Letting the database decide avoids the
        # read-then-write race between two consumer replicas.
        if _rows_affected(await self._session.execute(statement)) == 0:
            result.duplicates += 1
            return False

        result.applied += 1
        return True

    async def _charge(self, event: pb.UsageEvent, result: ApplyResult) -> None:
        """Add the request's price to every budget it names."""
        if not event.budget_ids or event.price_micro_usd <= 0:
            return

        for budget_id in event.budget_ids:
            # An UPDATE rather than a read-modify-write, so two consumer
            # replicas charging the same budget cannot lose one another's
            # increments.
            charged = await self._session.execute(
                update(models.Budget)
                .where(models.Budget.id == budget_id, models.Budget.tenant_id == event.tenant)
                .values(spent_micro_usd=models.Budget.spent_micro_usd + event.price_micro_usd)
            )
            if _rows_affected(charged) == 0:
                # The budget was deleted after the request was served. Counted
                # rather than raised: a deleted budget must not wedge the
                # consumer on a message it can never process.
                result.unknown_budgets += 1
            else:
                result.tenants_touched.add(event.tenant)

    async def _bump_versions(self, tenants: set[str]) -> None:
        """Advance each touched tenant's layer version.

        A worker rejects a layer whose version has not moved forward, so spend
        recorded without this would never reach the data plane and a budget
        would never actually deny anything.

        Once per batch rather than per event: every bump is a snapshot the
        fleet re-fetches, and one per request would make the propagation cost
        scale with traffic instead of with change.
        """
        for tenant_id in sorted(tenants):
            await self._session.execute(
                update(models.Tenant)
                .where(models.Tenant.id == tenant_id)
                .values(version=models.Tenant.version + 1)
            )

    async def spend_for(self, budget_id: str) -> int:
        """Read a budget's recorded spend, for tests and for the admin API."""
        budget = await self._session.get(models.Budget, budget_id)
        return budget.spent_micro_usd if budget is not None else 0

    async def record_count(self) -> int:
        """Count stored usage records."""
        rows = await self._session.scalars(select(models.UsageRecord.request_id))
        return len(list(rows))


def _rows_affected(result: object) -> int:
    """Read rowcount off a result.

    SQLAlchemy types execute() as returning Result, which does not declare
    rowcount even though every DML result has it. Reading it through getattr
    keeps the check honest without loosening the type checker for the module.
    """
    return int(getattr(result, "rowcount", 0))


def _upsert(session: AsyncSession):  # type: ignore[no-untyped-def]
    """Return the dialect's INSERT, which is where ON CONFLICT lives.

    SQLAlchemy's generic insert has no on_conflict_do_nothing; it is a
    dialect-specific construct. Both dialects we run on spell it the same way,
    so the branch is one line rather than two code paths.
    """
    name = session.bind.dialect.name if session.bind is not None else "postgresql"
    return (
        sqlite_insert(models.UsageRecord)
        if name == "sqlite"
        else postgres_insert(models.UsageRecord)
    )


def _from_unix_ms(ms: int) -> datetime:
    if ms == 0:
        return datetime.now(UTC)
    return datetime.fromtimestamp(ms / 1000, tz=UTC)
