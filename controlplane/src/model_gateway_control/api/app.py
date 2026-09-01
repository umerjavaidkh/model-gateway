"""The admin FastAPI application.

Note the absence of ``from __future__ import annotations``. FastAPI resolves
handler annotations at module scope, and with postponed evaluation the
dependency aliases defined inside the factory are unresolvable strings — which
surfaces as every request failing with "session: Field required" as though it
were a missing query parameter. The rest of this package uses the future import;
this module cannot.
"""

import json
import secrets
from collections.abc import AsyncIterator, Callable
from contextlib import asynccontextmanager
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Annotated, Any

from fastapi import Depends, FastAPI, Header, HTTPException, Query, Request, Response, status
from pydantic import BaseModel, Field
from sqlalchemy.ext.asyncio import AsyncEngine, AsyncSession

from model_gateway_control.api import idempotency
from model_gateway_control.db.repository import Repository
from model_gateway_control.db.session import session_factory
from model_gateway_control.domain.catalog import TrustTier
from model_gateway_control.errors import (
    ConflictError,
    ForbiddenError,
    GatewayError,
    InvalidRequestError,
    NotFoundError,
)
from model_gateway_control.service.keys import KeyService
from model_gateway_control.snapshot import build_snapshot

#: Gateway error to HTTP status. The only place that knows both, which is what
#: keeps HTTP out of the service layer. An unmapped error is a 500 by design:
#: it means somebody added a code without deciding what it means to a caller.
_STATUS = {
    InvalidRequestError: status.HTTP_400_BAD_REQUEST,
    ForbiddenError: status.HTTP_403_FORBIDDEN,
    NotFoundError: status.HTTP_404_NOT_FOUND,
    ConflictError: status.HTTP_409_CONFLICT,
}


@dataclass(frozen=True, slots=True, kw_only=True)
class AdminSettings:
    """What the admin application needs to run.

    Passed in rather than read from the environment here, so the app is
    constructible in a test without touching process state.
    """

    engine: AsyncEngine
    key_pepper: bytes
    #: Bearer token for the admin API. The in-process half of authentication;
    #: mTLS on the listener is the other half and is deployment configuration.
    admin_token: str
    now: Callable[[], datetime] | None = None


class IssueKeyRequest(BaseModel):
    """Issue a key for exactly one application or user."""

    key_id: str = Field(min_length=1, max_length=64)
    application_id: str | None = None
    user_id: str | None = None
    models_allow_all: bool = False
    min_trust_tier: str = "EXTERNAL"


class RotateKeyRequest(BaseModel):
    """Rotate a key, optionally naming its successor."""

    new_key_id: str | None = Field(default=None, max_length=64)


class KeyResponse(BaseModel):
    """A newly minted key.

    ``presented`` appears here once and is never stored. A caller that loses it
    rotates; there is no way to read it back, which is the property that makes
    a leaked database useless.
    """

    key_id: str
    presented: str


def create_app(settings: AdminSettings) -> FastAPI:
    """Build the admin application."""
    factory = session_factory(settings.engine)
    now = settings.now or (lambda: datetime.now(UTC))

    @asynccontextmanager
    async def lifespan(_: FastAPI) -> AsyncIterator[None]:
        yield
        await settings.engine.dispose()

    app = FastAPI(
        title="Model Gateway admin API",
        version="0.1.0",
        lifespan=lifespan,
    )

    async def authorize(authorization: Annotated[str | None, Header()] = None) -> None:
        """Check the admin bearer token in constant time.

        A plain equality check on a secret leaks its length and prefix through
        timing. The cost of doing it properly is one function call.
        """
        presented = ""
        if authorization and authorization.startswith("Bearer "):
            presented = authorization.removeprefix("Bearer ").strip()
        if not secrets.compare_digest(presented, settings.admin_token):
            raise HTTPException(status.HTTP_401_UNAUTHORIZED, "admin credentials required")

    async def get_session() -> AsyncIterator[AsyncSession]:
        async with factory() as session:
            yield session

    # Type aliases, which PEP 8 spells in CapWords. They live inside the factory
    # because both depend on this application's engine, and N806 only knows they
    # are assignments in a function body.
    Session = Annotated[AsyncSession, Depends(get_session)]  # noqa: N806
    Authorized = Depends(authorize)  # noqa: N806

    @app.exception_handler(GatewayError)
    async def _gateway_error(_: Request, err: GatewayError) -> Response:
        code = _STATUS.get(type(err), status.HTTP_500_INTERNAL_SERVER_ERROR)
        message = err.message if code < status.HTTP_500_INTERNAL_SERVER_ERROR else "internal error"
        return Response(
            content=f'{{"error":{{"code":"{err.code}","message":{_json_str(message)}}}}}',
            media_type="application/json",
            status_code=code,
        )

    @app.get("/healthz", dependencies=[])
    async def healthz() -> dict[str, str]:
        return {"status": "ok"}

    @app.post(
        "/v1/tenants/{tenant_id}/keys",
        status_code=status.HTTP_201_CREATED,
        dependencies=[Authorized],
    )
    async def issue_key(
        tenant_id: str,
        body: IssueKeyRequest,
        session: Session,
        dry_run: Annotated[bool, Query()] = False,
        idempotency_key: Annotated[str | None, Header(alias="Idempotency-Key")] = None,
    ) -> Response:
        endpoint = "issue_key"
        payload = body.model_dump() | {"tenant_id": tenant_id}

        if idempotency_key:
            replayed = await idempotency.replay(session, idempotency_key, endpoint, payload)
            if replayed is not None:
                return _json_response(*replayed)

        service = KeyService(session, settings.key_pepper, now=now)
        minted = await service.issue(
            tenant_id=tenant_id,
            key_id=body.key_id,
            application_id=body.application_id,
            user_id=body.user_id,
            models_allow_all=body.models_allow_all,
            min_trust_tier=_trust_tier(body.min_trust_tier),
        )

        if dry_run:
            # Validated against the real database and then thrown away, which is
            # what makes a dry run worth trusting. An agent uses this to check a
            # spec before committing to it.
            await session.rollback()
            return _json_response(status.HTTP_200_OK, {"dry_run": True, "key_id": minted.key_id})

        result = KeyResponse(key_id=minted.key_id, presented=minted.presented).model_dump()
        if idempotency_key:
            await idempotency.remember(
                session, idempotency_key, endpoint, payload, status.HTTP_201_CREATED, result
            )
        await session.commit()
        return _json_response(status.HTTP_201_CREATED, result)

    @app.post("/v1/keys/{key_id}/rotate", dependencies=[Authorized])
    async def rotate_key(
        key_id: str,
        body: RotateKeyRequest,
        session: Session,
        dry_run: Annotated[bool, Query()] = False,
        idempotency_key: Annotated[str | None, Header(alias="Idempotency-Key")] = None,
    ) -> Response:
        endpoint = "rotate_key"
        payload = body.model_dump() | {"key_id": key_id}

        if idempotency_key:
            replayed = await idempotency.replay(session, idempotency_key, endpoint, payload)
            if replayed is not None:
                return _json_response(*replayed)

        service = KeyService(session, settings.key_pepper, now=now)
        minted = await service.rotate(key_id, new_key_id=body.new_key_id)

        if dry_run:
            await session.rollback()
            return _json_response(status.HTTP_200_OK, {"dry_run": True, "key_id": minted.key_id})

        result = KeyResponse(key_id=minted.key_id, presented=minted.presented).model_dump()
        if idempotency_key:
            await idempotency.remember(
                session, idempotency_key, endpoint, payload, status.HTTP_200_OK, result
            )
        await session.commit()
        return _json_response(status.HTTP_200_OK, result)

    @app.delete(
        "/v1/keys/{key_id}", status_code=status.HTTP_204_NO_CONTENT, dependencies=[Authorized]
    )
    async def revoke_key(key_id: str, session: Session) -> Response:
        await KeyService(session, settings.key_pepper, now=now).revoke(key_id)
        await session.commit()
        return Response(status_code=status.HTTP_204_NO_CONTENT)

    @app.post("/v1/snapshots", dependencies=[Authorized])
    async def build(session: Session) -> Response:
        """Compile the current configuration and report what it produced.

        Returns the digests rather than the bytes so that a caller can tell
        whether anything changed without transferring a snapshot. The bytes are
        served separately.
        """
        repo = Repository(session)
        snapshot = build_snapshot(await repo.load_fleet(), await repo.load_tenants(), now())
        return _json_response(
            status.HTTP_200_OK,
            {
                "global_version": snapshot.global_layer.version.number,
                "global_digest": snapshot.global_layer.version.digest,
                "tenants": [
                    {
                        "tenant": layer.tenant,
                        "version": layer.version.number,
                        "digest": layer.version.digest,
                    }
                    for layer in snapshot.tenants
                ],
            },
        )

    @app.get("/v1/snapshots/current", dependencies=[Authorized])
    async def current(session: Session) -> Response:
        """Serve the compiled snapshot as protobuf.

        This is what the worker-side subscriber will fetch. Serving it here now
        means the subscriber has something real to poll before the watch stream
        exists.
        """
        repo = Repository(session)
        snapshot = build_snapshot(await repo.load_fleet(), await repo.load_tenants(), now())
        return Response(
            content=snapshot.SerializeToString(deterministic=True),
            media_type="application/x-protobuf",
            headers={"X-Snapshot-Digest": snapshot.global_layer.version.digest},
        )

    return app


def _trust_tier(name: str) -> TrustTier:
    try:
        return TrustTier[name.upper()]
    except KeyError:
        raise InvalidRequestError(f"unknown trust tier {name!r}") from None


def _json_response(code: int, body: Any) -> Response:
    return Response(content=json.dumps(body), media_type="application/json", status_code=code)


def _json_str(value: str) -> str:
    return json.dumps(value)
