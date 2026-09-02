"""Read usage events from the Redis stream.

A consumer group is used rather than a plain read, so several replicas share
the work and an unacknowledged message is redelivered rather than lost. That
redelivery is precisely why the accountant is idempotent: at-least-once is the
guarantee the stream offers, and the consumer has to be built for it rather
than hope for exactly-once.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import Any

from redis.asyncio import Redis
from redis.exceptions import ResponseError
from redis.exceptions import TimeoutError as RedisTimeoutError

from model_gateway_control.wire import usage_pb2 as pb

#: Must match the data plane's redisstream.DefaultStream and PayloadField.
#: They are constants in two languages rather than a shared schema because a
#: key name is not a message; the messages themselves are generated.
DEFAULT_STREAM = "gateway:usage"
PAYLOAD_FIELD = b"event"
DEFAULT_GROUP = "accounting"

logger = logging.getLogger(__name__)


@dataclass(frozen=True, slots=True)
class Batch:
    """Events read together, with the ids needed to acknowledge them."""

    ids: list[bytes]
    events: list[pb.UsageEvent]

    def __len__(self) -> int:
        return len(self.events)


class UsageStream:
    """A consumer-group reader over the usage stream."""

    def __init__(
        self,
        client: Redis,
        *,
        stream: str = DEFAULT_STREAM,
        group: str = DEFAULT_GROUP,
        consumer: str = "accounting-1",
    ) -> None:
        self._client = client
        self._stream = stream
        self._group = group
        self._consumer = consumer

    async def ensure_group(self) -> None:
        """Create the consumer group if it does not exist.

        ``mkstream`` so the consumer can start before any worker has published,
        which is the normal order in a fresh deployment. An existing group is
        not an error — every replica calls this at startup.
        """
        try:
            await self._client.xgroup_create(self._stream, self._group, id="0", mkstream=True)
        except ResponseError as err:
            if "BUSYGROUP" not in str(err):
                raise

    async def read(self, *, count: int = 100, block_ms: int = 5000) -> Batch:
        """Read the next batch of undelivered events.

        Blocks rather than polling, so an idle consumer costs one connection
        instead of a query every interval.
        """
        try:
            response = await self._client.xreadgroup(
                groupname=self._group,
                consumername=self._consumer,
                streams={self._stream: ">"},
                count=count,
                block=block_ms,
            )
        except RedisTimeoutError:
            # A blocking read that found nothing. redis-py raises rather than
            # returning empty when the block window elapses, so treating this
            # as an error would kill the consumer on its first idle window —
            # and an accounting consumer is idle most of the time.
            #
            # Not caught in tests because a test always has events waiting.
            # Found by running the consumer against a quiet Redis, which is
            # what production looks like at four in the morning.
            return Batch(ids=[], events=[])
        return self._decode(response)

    async def read_pending(self, *, count: int = 100) -> Batch:
        """Re-read messages delivered to this consumer but never acknowledged.

        A consumer that died mid-batch leaves them owned but unacknowledged;
        without this they would sit in the pending list forever and their spend
        would never be recorded.
        """
        response = await self._client.xreadgroup(
            groupname=self._group,
            consumername=self._consumer,
            streams={self._stream: "0"},
            count=count,
        )
        return self._decode(response)

    async def ack(self, ids: list[bytes]) -> None:
        """Acknowledge processed messages."""
        if ids:
            await self._client.xack(self._stream, self._group, *ids)

    def _decode(self, response: Any) -> Batch:
        """Turn a raw xreadgroup response into a batch.

        The response shape depends on the RESP protocol version: RESP2 gives a
        list of (stream, entries) pairs, RESP3 gives a mapping. The client pins
        RESP2, so in practice only the first arrives — but both are handled,
        because the cost is two lines and the alternative is a crash if anyone
        ever changes the pin.
        """
        ids: list[bytes] = []
        events: list[pb.UsageEvent] = []

        for entries in _entries_by_stream(response):
            for entry_id, fields in entries:
                payload = fields.get(PAYLOAD_FIELD)
                if payload is None:
                    # Nothing to decode. Acknowledged anyway, because leaving
                    # it pending would redeliver it forever.
                    ids.append(entry_id)
                    continue

                event = pb.UsageEvent()
                try:
                    event.ParseFromString(payload)
                except Exception:
                    # A malformed event is dropped with a log rather than
                    # stopping the consumer: one bad message must not halt
                    # accounting for everything behind it.
                    logger.exception(
                        "discarding an unparseable usage event", extra={"id": entry_id}
                    )
                    ids.append(entry_id)
                    continue

                ids.append(entry_id)
                events.append(event)

        return Batch(ids=ids, events=events)


def _entries_by_stream(response: Any) -> list[Any]:
    """Extract the per-stream entry lists from either response shape."""
    if not response:
        return []
    if isinstance(response, dict):
        return list(response.values())
    return [entries for _stream, entries in response]
