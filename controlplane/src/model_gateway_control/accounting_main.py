"""Run the accounting consumer.

A separate process from the admin API, because it is a long-running loop with
different failure characteristics: the API being down blocks configuration
changes, this being down only means budgets stop advancing while the record of
what was spent keeps accumulating in the stream.
"""

from __future__ import annotations

import asyncio
import logging
import os
import signal

from redis.asyncio import Redis
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker

from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.errors import InvalidRequestError
from model_gateway_control.service.accounting import Accountant
from model_gateway_control.service.usage_stream import Batch, UsageStream

logger = logging.getLogger("accounting")

DEFAULT_BATCH = 200
DEFAULT_BLOCK_MS = 5000


async def run(
    database_url: str, redis_url: str, consumer: str, *, stop: asyncio.Event | None = None
) -> None:
    """Consume until stopped."""
    stop = stop or asyncio.Event()

    engine = create_engine(database_url)
    factory = session_factory(engine)
    # RESP2 explicitly rather than by negotiation. The response shape of
    # XREADGROUP differs between protocol versions, and which one a client ends
    # up on depends on the server's HELLO — so leaving it implicit means the
    # parser that runs is decided by whatever Redis the deployment happens to
    # have. Pinning it makes the shape a property of this code.
    client: Redis = Redis.from_url(redis_url, protocol=2)
    stream = UsageStream(client, consumer=consumer)

    try:
        await stream.ensure_group()

        # Anything this consumer was handed before it died is still owned by it
        # and unacknowledged. Draining that first means a restart does not
        # strand spend in the pending list forever.
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
    factory: async_sessionmaker[AsyncSession], stream: UsageStream, batch: Batch
) -> None:
    """Record a batch, then acknowledge it.

    In that order, deliberately. Acknowledging first would lose the batch if
    the write failed; this way a crash between the two redelivers events the
    accountant already discards as duplicates. At-least-once with an idempotent
    consumer is the only combination that loses nothing and counts nothing
    twice.
    """
    async with factory() as session:
        result = await Accountant(session).apply(batch.events)
        await session.commit()

    logger.info(
        "applied usage batch",
        extra={
            "applied": result.applied,
            "duplicates": result.duplicates,
            "unknown_budgets": result.unknown_budgets,
            "tenants": len(result.tenants_touched),
        },
    )
    await stream.ack(batch.ids)


def main() -> int:
    """Start the consumer. Returns a process exit code."""
    logging.basicConfig(level=logging.INFO)

    database_url = os.environ.get("GATEWAY_DATABASE_URL", "")
    redis_url = os.environ.get("GATEWAY_REDIS_URL", "")
    consumer = os.environ.get("GATEWAY_CONSUMER_NAME", "accounting-1")

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
