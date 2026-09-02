"""Reading what requests did, for whoever has to explain one.

The usage stream exists for accounting, and accounting only ever asks "how
much". This asks the other questions: which requests failed, what did the last
hundred do, and where exactly did this one end.

Every field it reads was already on the event. The consumer was storing eight
of them and dropping the rest, so answering "why was this slow" meant opening a
trace — which most deployments do not have a backend for on day one.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import datetime
from typing import Any

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.errors import NotFoundError

#: A ceiling on any listing. A dashboard that can ask for a million rows will,
#: eventually, from a browser tab somebody left open.
MAX_LIMIT = 500


@dataclass(frozen=True, slots=True)
class Stage:
    """One leg of the request path and what it cost."""

    name: str
    duration_ms: int
    #: Empty when the stage passed, otherwise the code it refused with. The
    #: stage that refused is where the request ended.
    outcome: str


@dataclass(frozen=True, slots=True)
class RequestRecord:
    """What one request did."""

    request_id: str
    occurred_at: datetime
    tenant: str
    key_id: str
    deployment: str
    base_model: str
    adapter_id: str
    provider: str
    stream: bool
    shadow: bool
    outcome: str
    latency_ms: int
    time_to_first_byte_ms: int
    input_tokens: int
    output_tokens: int
    cost_micro_usd: int
    price_micro_usd: int
    snapshot_version: int
    stages: tuple[Stage, ...]

    @property
    def failed(self) -> bool:
        """Whether the gateway refused it or the upstream did."""
        return bool(self.outcome)

    @property
    def failed_at(self) -> str:
        """Which stage ended it, or empty when it succeeded.

        The question somebody actually has when a request failed. The final
        code says *what* went wrong; this says *where*, which is what decides
        who looks at it next.
        """
        for stage in self.stages:
            if stage.outcome:
                return stage.name
        return ""


class RequestLog:
    """Queries over what traffic did."""

    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    async def recent(
        self,
        *,
        limit: int = 100,
        failed_only: bool = False,
        tenant: str | None = None,
        include_shadow: bool = False,
    ) -> list[RequestRecord]:
        """The most recent requests, newest first.

        Shadow traffic is excluded unless asked for: mirrored requests nobody
        was waiting for would otherwise crowd out the ones somebody was, and
        their failures are a finding about an adapter rather than an incident.
        """
        query = select(models.UsageRecord).order_by(models.UsageRecord.occurred_at.desc())
        if failed_only:
            query = query.where(models.UsageRecord.outcome != "")
        if tenant is not None:
            query = query.where(models.UsageRecord.tenant_id == tenant)
        if not include_shadow:
            query = query.where(models.UsageRecord.shadow.is_(False))

        query = query.limit(max(1, min(limit, MAX_LIMIT)))
        return [_to_record(row) for row in (await self._session.scalars(query)).all()]

    async def get(self, request_id: str) -> RequestRecord:
        """One request, with everything known about it."""
        row = await self._session.get(models.UsageRecord, request_id)
        if row is None:
            raise NotFoundError(f"no request {request_id!r}")
        return _to_record(row)

    async def failure_summary(self, limit: int = 500) -> dict[str, int]:
        """How many of the recent failures were of each kind.

        Counted over a bounded window rather than the whole table: the question
        is "what is going wrong now", and a lifetime total answers a different
        one while getting slower every day.
        """
        query = (
            select(models.UsageRecord.outcome)
            .where(models.UsageRecord.outcome != "")
            .order_by(models.UsageRecord.occurred_at.desc())
            .limit(max(1, min(limit, MAX_LIMIT)))
        )
        counts: dict[str, int] = {}
        for outcome in (await self._session.scalars(query)).all():
            counts[outcome] = counts.get(outcome, 0) + 1
        return dict(sorted(counts.items(), key=lambda kv: -kv[1]))


def _to_record(row: models.UsageRecord) -> RequestRecord:
    return RequestRecord(
        request_id=row.request_id,
        occurred_at=row.occurred_at,
        tenant=row.tenant_id,
        key_id=row.key_id,
        deployment=row.deployment,
        base_model=row.base_model,
        adapter_id=row.adapter_id,
        provider=row.provider,
        stream=row.stream,
        shadow=row.shadow,
        outcome=row.outcome,
        latency_ms=row.latency_ms,
        time_to_first_byte_ms=row.time_to_first_byte_ms,
        input_tokens=row.input_tokens,
        output_tokens=row.output_tokens,
        cost_micro_usd=row.cost_micro_usd,
        price_micro_usd=row.price_micro_usd,
        snapshot_version=row.snapshot_version,
        stages=_to_stages(row.stages),
    )


def _to_stages(raw: str) -> tuple[Stage, ...]:
    """Stage timings, tolerating a record written before they existed."""
    if not raw:
        return ()
    decoded: list[dict[str, Any]] = json.loads(raw)
    return tuple(
        Stage(
            name=str(s.get("name", "")),
            duration_ms=int(s.get("duration_ms", 0)),
            outcome=str(s.get("outcome", "")),
        )
        for s in decoded
    )
