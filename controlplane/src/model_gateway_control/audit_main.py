"""Run the audit consumer.

A separate process from accounting, and a single one.

**Separate**, because the two have different failure consequences. Accounting
falling behind delays an invoice; audit falling behind leaves decisions
unrecorded while the gateway keeps making them. Sharing a process means one
bad batch stops both, and it means scaling accounting scales audit — which is
the one thing audit must not do.

**Single**, because the chain is serial. Two appenders reading the same head
produce two records claiming the same predecessor, and a forked chain verifies
as broken with no way to tell which half is real. The advisory lock in the
appender makes a second replica safe rather than correct: it will not corrupt
the chain, it will simply take turns. Running one is the intent; the lock is
there because "we only ever run one" is not something a deployment enforces.
"""

from __future__ import annotations

import asyncio
import logging
import os
import signal
from datetime import UTC, datetime

from redis.asyncio import Redis
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker

from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.errors import InvalidRequestError
from model_gateway_control.service.audit import AuditLog, Entry
from model_gateway_control.service.usage_stream import AuditStream, Batch
from model_gateway_control.wire import usage_pb2 as pb

#: Smaller than the accounting batch. The batch is one held lock and one
#: transaction, and a long one blocks nothing else — but it also delays the
#: point at which these records become durable, and audit is the data where
#: that matters most.
DEFAULT_BATCH = 50
DEFAULT_BLOCK_MS = 5000

logger = logging.getLogger(__name__)


async def run(
    database_url: str, redis_url: str, consumer: str, *, stop: asyncio.Event | None = None
) -> None:
    """Consume until stopped."""
    stop = stop or asyncio.Event()

    engine = create_engine(database_url)
    factory = session_factory(engine)
    # RESP2 explicitly, for the same reason accounting pins it: the shape of an
    # XREADGROUP response differs between protocol versions, and leaving it to
    # negotiation means the parser that runs is chosen by whichever Redis the
    # deployment happens to have.
    client: Redis = Redis.from_url(redis_url, protocol=2)
    stream = AuditStream(client, consumer=consumer)

    try:
        await stream.ensure_group()

        # Anything handed to this consumer before it died is still owned by it.
        # For audit that matters more than for accounting: an unacknowledged
        # decision is one the chain does not contain.
        pending = await stream.read_pending(count=DEFAULT_BATCH)
        if pending.events:
            await _apply(factory, stream, pending)

        while not stop.is_set():
            batch = await stream.read(count=DEFAULT_BATCH, block_ms=DEFAULT_BLOCK_MS)
            if not batch.ids:
                continue
            await _apply(factory, stream, batch)
    finally:
        await client.aclose()
        await engine.dispose()


async def _apply(
    factory: async_sessionmaker[AsyncSession], stream: AuditStream, batch: Batch[pb.AuditEvent]
) -> None:
    """Append a batch, then acknowledge it.

    In that order. Acknowledging first would lose the batch if the write
    failed, and a lost audit record is the failure this whole module exists to
    make impossible; a crash between the two redelivers records the appender
    already recognises by their event id and skips.
    """
    async with factory() as session:
        result = await AuditLog(session).append([_entry(event) for event in batch.events])
        await session.commit()

    logger.info(
        "appended audit batch",
        extra={"appended": result.appended, "duplicates": result.duplicates},
    )
    await stream.ack(batch.ids)


def _entry(event: pb.AuditEvent) -> Entry:
    return Entry(
        event_id=event.event_id,
        action=event.action,
        occurred_at=datetime.fromtimestamp(event.timestamp_unix_ms / 1000, tz=UTC),
        request_id=event.request_id,
        tenant=event.tenant,
        actor=event.actor,
        resource=event.resource,
        outcome=event.outcome,
        reason=event.reason,
        source_ip=event.source_ip,
        snapshot_version=event.snapshot_version,
    )


def main() -> int:
    """Start the consumer. Returns a process exit code."""
    logging.basicConfig(level=logging.INFO)

    database_url = os.environ.get("GATEWAY_DATABASE_URL", "")
    redis_url = os.environ.get("GATEWAY_REDIS_URL", "")
    consumer = os.environ.get("GATEWAY_CONSUMER_NAME", "audit-1")

    if not database_url:
        raise InvalidRequestError("GATEWAY_DATABASE_URL is required")
    if not redis_url:
        raise InvalidRequestError("GATEWAY_REDIS_URL is required")

    async def serve() -> None:
        stop = asyncio.Event()
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.add_signal_handler(sig, stop.set)
        await run(database_url, redis_url, consumer, stop=stop)

    asyncio.run(serve())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
