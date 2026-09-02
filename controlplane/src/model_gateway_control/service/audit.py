"""Appending to the audit chain, and checking it afterwards.

# Why this is deliberately not concurrent

A hash chain is a serial structure. Two appenders that both read the head, both
compute a hash against it and both insert produce two records claiming the same
predecessor — a fork, which verifies as a break and cannot be repaired without
deciding which half of the history to discard.

So appending takes a lock, and the lock is over the chain rather than over a
row: the contended resource is *the position at the end*, which no existing row
represents. On Postgres that is a transaction-scoped advisory lock, released
when the transaction ends however it ends. On SQLite, where tests run, writes
are already serialised by the database and the lock is a no-op.

The cost is that audit ingestion does not scale horizontally. That is the
correct trade for this data: the volume is refusals and configuration changes
rather than every request, and a chain that scaled by forking would be a chain
that proves nothing.

# Idempotency

Delivery is at-least-once, so a redelivered event must not extend the chain a
second time. The event id is unique, and an insert that conflicts with an
existing one is skipped — checked before the hash is computed, so a duplicate
costs a lookup rather than a wasted position in the sequence.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from datetime import datetime

from sqlalchemy import func, select, text
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.db.timestamps import as_utc
from model_gateway_control.domain.audit import GENESIS, Link, compute_hash

logger = logging.getLogger(__name__)

#: A ceiling on any listing, for the same reason the request log has one: a
#: page that can ask for a million rows eventually will.
MAX_LIMIT = 500

#: Identifies the advisory lock. Any constant works as long as nothing else in
#: this database picks the same one, so it is written down here rather than
#: computed from a name whose hash could collide with another module's.
_CHAIN_LOCK_ID = 0x4155_4449  # "AUDI"


@dataclass(frozen=True, slots=True)
class Appended:
    """What one batch did."""

    appended: int = 0
    duplicates: int = 0

    def __len__(self) -> int:
        return self.appended


@dataclass(frozen=True, slots=True)
class Verdict:
    """Whether the chain verifies, and where it stops if it does not."""

    checked: int
    #: The sequence number of the first record whose hash does not match, or
    #: None when every record does.
    broken_at: int | None = None
    #: Why it does not match, in the terms an operator can act on.
    reason: str = ""
    #: The hash of the last record checked. This is the value worth publishing
    #: somewhere the writer cannot reach: comparing today's head against a copy
    #: made yesterday detects a rewrite that recomputed the whole chain, which
    #: no amount of internal checking can.
    head: str = GENESIS

    @property
    def intact(self) -> bool:
        """True when every record checked hashes to what it claims."""
        return self.broken_at is None


@dataclass(frozen=True, slots=True, kw_only=True)
class Entry:
    """One decision to append, before it has a position in the chain."""

    event_id: str
    action: str
    occurred_at: datetime
    request_id: str = ""
    tenant: str = ""
    actor: str = ""
    resource: str = ""
    outcome: str = ""
    reason: str = ""
    source_ip: str = ""
    snapshot_version: int = 0
    #: Kept out of the hash and out of the table. Present so a caller can pass
    #: an entry around with context attached without that context silently
    #: becoming part of what the chain commits to.
    context: dict[str, str] = field(default_factory=dict)


class AuditLog:
    """Appends to the chain and reads it back."""

    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    async def append(self, entries: list[Entry]) -> Appended:
        """Append a batch, in the order given.

        The caller commits. Holding the lock across the whole batch rather than
        per record means a batch is one contended section instead of many, and
        it is what makes the sequence numbers within a batch contiguous.
        """
        if not entries:
            return Appended()

        await self._lock()
        seq, prev_hash = await self._head()

        appended = 0
        duplicates = 0
        for entry in entries:
            if await self._seen(entry.event_id):
                duplicates += 1
                continue

            seq += 1
            link = Link(
                seq=seq,
                event_id=entry.event_id,
                request_id=entry.request_id,
                occurred_at=as_utc(entry.occurred_at),
                tenant=entry.tenant,
                actor=entry.actor,
                action=entry.action,
                resource=entry.resource,
                outcome=entry.outcome,
                reason=entry.reason,
                source_ip=entry.source_ip,
                snapshot_version=entry.snapshot_version,
            )
            digest = compute_hash(link, prev_hash)
            self._session.add(
                models.AuditRecord(
                    seq=seq,
                    event_id=link.event_id,
                    request_id=link.request_id,
                    occurred_at=link.occurred_at,
                    tenant_id=link.tenant,
                    actor=link.actor,
                    action=link.action,
                    resource=link.resource,
                    outcome=link.outcome,
                    reason=link.reason,
                    source_ip=link.source_ip,
                    snapshot_version=link.snapshot_version,
                    prev_hash=prev_hash,
                    hash=digest,
                )
            )
            prev_hash = digest
            appended += 1

        # Within the lock, so the next appender reads a head that includes
        # these rather than a position they are about to take.
        await self._session.flush()
        return Appended(appended=appended, duplicates=duplicates)

    async def verify(self, *, limit: int | None = None) -> Verdict:
        """Recompute the chain and report the first record that does not match.

        Walks in sequence order from the beginning, because a chain checked
        from the middle can only prove the middle: the whole point is that a
        record's hash depends on everything before it.
        """
        statement = select(models.AuditRecord).order_by(models.AuditRecord.seq)
        if limit is not None:
            statement = statement.limit(limit)
        rows = (await self._session.execute(statement)).scalars().all()

        prev_hash = GENESIS
        for row in rows:
            if row.prev_hash != prev_hash:
                # The link is wrong before the content is even hashed, which is
                # what a deleted row looks like: the survivors are individually
                # valid and no longer point at each other.
                return Verdict(
                    checked=len(rows),
                    broken_at=row.seq,
                    reason=(
                        f"record {row.seq} follows {row.prev_hash[:12]}… but the "
                        f"previous record hashes to {prev_hash[:12]}… — a record "
                        "between them is missing or was changed"
                    ),
                    head=prev_hash,
                )

            expected = compute_hash(_link_of(row), prev_hash)
            if expected != row.hash:
                return Verdict(
                    checked=len(rows),
                    broken_at=row.seq,
                    reason=(
                        f"record {row.seq} hashes to {expected[:12]}… but stores "
                        f"{row.hash[:12]}… — its contents were changed after it "
                        "was written"
                    ),
                    head=prev_hash,
                )
            prev_hash = row.hash

        return Verdict(checked=len(rows), head=prev_hash)

    async def recent(
        self,
        *,
        limit: int = 100,
        tenant: str | None = None,
        action: str | None = None,
        refusals_only: bool = False,
    ) -> list[models.AuditRecord]:
        """The most recent records, newest first."""
        statement = select(models.AuditRecord).order_by(models.AuditRecord.seq.desc())
        if tenant:
            statement = statement.where(models.AuditRecord.tenant_id == tenant)
        if action:
            statement = statement.where(models.AuditRecord.action == action)
        if refusals_only:
            statement = statement.where(models.AuditRecord.outcome != "")
        statement = statement.limit(min(max(limit, 1), MAX_LIMIT))
        return list((await self._session.execute(statement)).scalars().all())

    async def action_summary(self) -> dict[str, int]:
        """How many records each action has, most frequent first."""
        rows = await self._session.execute(
            select(models.AuditRecord.action, func.count())
            .group_by(models.AuditRecord.action)
            .order_by(func.count().desc())
        )
        return dict(rows.all())  # type: ignore[arg-type]

    async def _lock(self) -> None:
        """Serialise appenders against each other.

        Transaction-scoped, so it is released by commit or rollback and a
        crashed appender cannot hold the chain closed.
        """
        if self._session.bind is None or self._session.bind.dialect.name != "postgresql":
            # SQLite serialises writers already, and no other dialect is
            # supported. Silently doing nothing elsewhere would be a fork
            # waiting for a deployment nobody tested.
            return
        await self._session.execute(
            text("SELECT pg_advisory_xact_lock(:id)"), {"id": _CHAIN_LOCK_ID}
        )

    async def _head(self) -> tuple[int, str]:
        """The last record's sequence number and hash, or the genesis values."""
        row = (
            await self._session.execute(
                select(models.AuditRecord.seq, models.AuditRecord.hash)
                .order_by(models.AuditRecord.seq.desc())
                .limit(1)
            )
        ).first()
        return (0, GENESIS) if row is None else (row[0], row[1])

    async def _seen(self, event_id: str) -> bool:
        found = await self._session.execute(
            select(models.AuditRecord.seq).where(models.AuditRecord.event_id == event_id).limit(1)
        )
        return found.first() is not None


def _link_of(row: models.AuditRecord) -> Link:
    return Link(
        seq=row.seq,
        event_id=row.event_id,
        request_id=row.request_id,
        occurred_at=as_utc(row.occurred_at),
        tenant=row.tenant_id,
        actor=row.actor,
        action=row.action,
        resource=row.resource,
        outcome=row.outcome,
        reason=row.reason,
        source_ip=row.source_ip,
        snapshot_version=row.snapshot_version,
    )
