"""Replay protection for mutations.

An agent drives this API, and an agent that cannot tell a timeout from a failure
will retry. Without this, a retried "issue a key" issues two and the first is
never returned to anyone.

The request body is fingerprinted alongside the key, so reusing a key with a
*different* body is a conflict rather than a silent replay of the wrong
response — which would be worse than no idempotency at all.
"""

from __future__ import annotations

import hashlib
import json
from typing import Any

from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.errors import ConflictError


def fingerprint(payload: Any) -> str:
    """Hash a request body, stably regardless of key order."""
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"), default=str)
    return hashlib.sha256(encoded.encode()).hexdigest()


async def replay(
    session: AsyncSession, key: str, endpoint: str, request: Any
) -> tuple[int, Any] | None:
    """Return the stored response for a repeated request, if there is one.

    Raises ConflictError when the same key arrives with a different body: the
    caller has a bug, and answering with the earlier response would hide it.
    """
    record = await session.get(models.IdempotencyRecord, (key, endpoint))
    if record is None:
        return None
    if record.request_fingerprint != fingerprint(request):
        raise ConflictError(
            f"idempotency key {key!r} was already used with a different request body"
        )
    return record.status_code, json.loads(record.response_body)


async def remember(
    session: AsyncSession, key: str, endpoint: str, request: Any, status: int, response: Any
) -> None:
    """Store a completed response so a retry returns it instead of repeating."""
    session.add(
        models.IdempotencyRecord(
            key=key,
            endpoint=endpoint,
            request_fingerprint=fingerprint(request),
            status_code=status,
            response_body=json.dumps(response, default=str),
        )
    )
