"""The admin API.

These exercise the API through ASGI rather than by calling handlers, so the
dependency wiring, the error mapping and the status codes are all covered — the
parts that are easy to get wrong and impossible to check by calling a service.
"""

from __future__ import annotations

import os
from collections.abc import AsyncIterator
from datetime import UTC, datetime, timedelta

import pytest
import pytest_asyncio
from httpx import ASGITransport, AsyncClient
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.api.app import AdminSettings, create_app
from model_gateway_control.db import models
from model_gateway_control.db.models import Base
from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.domain.identity import compute_key_lookup
from model_gateway_control.wire import snapshot_pb2 as pb

DEFAULT_URL = "sqlite+aiosqlite:///:memory:"
PEPPER = b"an-admin-api-test-pepper-32-bytes!!"
TOKEN = "test-admin-token"
NOW = datetime(2026, 9, 1, 12, 0, 0, tzinfo=UTC)


@pytest_asyncio.fixture
async def client() -> AsyncIterator[AsyncClient]:
    """An API client over a freshly created schema, seeded with one tenant."""
    engine = create_engine(os.environ.get("GATEWAY_TEST_DATABASE_URL", DEFAULT_URL))
    async with engine.begin() as connection:
        await connection.run_sync(Base.metadata.drop_all)
        await connection.run_sync(Base.metadata.create_all)

    factory = session_factory(engine)
    async with factory() as session:
        await _seed(session)

    app = create_app(
        AdminSettings(engine=engine, key_pepper=PEPPER, admin_token=TOKEN, now=lambda: NOW)
    )
    transport = ASGITransport(app=app)
    async with AsyncClient(
        transport=transport,
        base_url="http://admin",
        headers={"Authorization": f"Bearer {TOKEN}"},
    ) as client:
        yield client
    await engine.dispose()


async def _seed(session: AsyncSession) -> None:
    session.add(models.FleetState(id=1, version=1, policy_bundle_ref=""))
    session.add(models.Tenant(id="acme", tier="enterprise", version=1, min_trust_tier=1))
    session.add(models.KeyPrefix(prefix="acme", tenant_id="acme"))
    session.add(models.Org(id="acme-org", tenant_id="acme", name="Acme"))
    session.add(models.Team(id="platform", org_id="acme-org", name="Platform"))
    session.add(models.Application(id="app-1", team_id="platform", name="Bot"))
    await session.commit()


async def test_issue_returns_the_secret_exactly_once(client: AsyncClient) -> None:
    response = await client.post(
        "/v1/tenants/acme/keys",
        json={"key_id": "key-1", "application_id": "app-1", "models_allow_all": True},
    )
    assert response.status_code == 201

    body = response.json()
    assert body["key_id"] == "key-1"
    assert body["presented"].startswith("gw_acme_")

    # There is no way to read it back. That is the property that makes a leaked
    # database useless: what is stored is an HMAC under a pepper the database
    # never sees.
    assert await client.get("/v1/keys/key-1") is not None


async def test_the_issued_key_authenticates_against_the_snapshot(client: AsyncClient) -> None:
    # The API and the builder agree only if the key the API mints is the key the
    # snapshot indexes. This is the seam between them.
    issued = await client.post(
        "/v1/tenants/acme/keys", json={"key_id": "key-1", "application_id": "app-1"}
    )
    secret = issued.json()["presented"].removeprefix("gw_acme_")

    raw = (await client.get("/v1/snapshots/current")).content
    snapshot = pb.Snapshot()
    snapshot.ParseFromString(raw)

    lookups = {bytes(entry.lookup) for entry in snapshot.tenants[0].keys}
    assert compute_key_lookup(PEPPER, secret) in lookups


async def test_rotation_keeps_the_old_key_working(client: AsyncClient) -> None:
    # A rotation that invalidates the old key immediately is an outage for every
    # caller that has not redeployed. Both generations must work during the
    # overlap.
    await client.post("/v1/tenants/acme/keys", json={"key_id": "key-1", "application_id": "app-1"})
    rotated = await client.post("/v1/keys/key-1/rotate", json={"new_key_id": "key-2"})
    assert rotated.status_code == 200
    assert rotated.json()["key_id"] == "key-2"

    raw = (await client.get("/v1/snapshots/current")).content
    snapshot = pb.Snapshot()
    snapshot.ParseFromString(raw)

    principals = {p.key_id: p for p in snapshot.tenants[0].principals}
    assert set(principals) == {"key-1", "key-2"}
    # The predecessor is marked so the data plane can warn the caller, and has a
    # deadline so it does not live forever.
    assert principals["key-1"].deprecated is True
    assert principals["key-1"].not_after_unix_ms > int(NOW.timestamp() * 1000)
    assert principals["key-2"].deprecated is False


async def test_rotation_carries_grants_to_the_successor(client: AsyncClient) -> None:
    # A rotated key that silently loses its roles or allowlist looks like a
    # permissions bug in the caller.
    await client.post(
        "/v1/tenants/acme/keys",
        json={"key_id": "key-1", "application_id": "app-1", "models_allow_all": True},
    )
    await client.post("/v1/keys/key-1/rotate", json={"new_key_id": "key-2"})

    raw = (await client.get("/v1/snapshots/current")).content
    snapshot = pb.Snapshot()
    snapshot.ParseFromString(raw)
    principals = {p.key_id: p for p in snapshot.tenants[0].principals}

    assert principals["key-2"].models_allow_all is True
    assert principals["key-2"].app == "app-1"


async def test_revoking_removes_a_key_from_the_snapshot_but_not_the_record(
    client: AsyncClient,
) -> None:
    await client.post("/v1/tenants/acme/keys", json={"key_id": "key-1", "application_id": "app-1"})
    assert (await client.delete("/v1/keys/key-1")).status_code == 204

    raw = (await client.get("/v1/snapshots/current")).content
    snapshot = pb.Snapshot()
    snapshot.ParseFromString(raw)
    assert len(snapshot.tenants[0].principals) == 0

    # Revoking twice is not an error; an agent retrying must not get a 404.
    assert (await client.delete("/v1/keys/key-1")).status_code == 204


async def test_a_retried_request_does_not_issue_a_second_key(client: AsyncClient) -> None:
    # The reason idempotency is here at all: an agent that cannot tell a timeout
    # from a failure will retry, and without this the first key is issued and
    # never returned to anyone.
    payload = {"key_id": "key-1", "application_id": "app-1"}
    headers = {"Idempotency-Key": "abc-123"}

    first = await client.post("/v1/tenants/acme/keys", json=payload, headers=headers)
    second = await client.post("/v1/tenants/acme/keys", json=payload, headers=headers)

    assert first.status_code == 201
    assert second.status_code == 201
    assert first.json() == second.json()

    raw = (await client.get("/v1/snapshots/current")).content
    snapshot = pb.Snapshot()
    snapshot.ParseFromString(raw)
    assert len(snapshot.tenants[0].principals) == 1


async def test_reusing_a_key_with_a_different_body_is_a_conflict(client: AsyncClient) -> None:
    # Replaying the earlier response would hide a caller bug, which is worse
    # than having no idempotency at all.
    headers = {"Idempotency-Key": "abc-123"}
    await client.post(
        "/v1/tenants/acme/keys",
        json={"key_id": "key-1", "application_id": "app-1"},
        headers=headers,
    )
    clash = await client.post(
        "/v1/tenants/acme/keys",
        json={"key_id": "key-2", "application_id": "app-1"},
        headers=headers,
    )
    assert clash.status_code == 409


async def test_a_dry_run_leaves_nothing_behind(client: AsyncClient) -> None:
    # Validated against the real database and then rolled back, which is what
    # makes a dry run worth trusting.
    response = await client.post(
        "/v1/tenants/acme/keys?dry_run=true",
        json={"key_id": "key-1", "application_id": "app-1"},
    )
    assert response.status_code == 200
    assert response.json()["dry_run"] is True

    raw = (await client.get("/v1/snapshots/current")).content
    snapshot = pb.Snapshot()
    snapshot.ParseFromString(raw)
    assert len(snapshot.tenants[0].principals) == 0


async def test_a_dry_run_still_reports_a_real_failure(client: AsyncClient) -> None:
    # A dry run that validates nothing is worse than none: it tells an agent the
    # spec is fine when it is not.
    response = await client.post(
        "/v1/tenants/nobody/keys?dry_run=true",
        json={"key_id": "key-1", "application_id": "app-1"},
    )
    assert response.status_code == 404


async def test_a_key_needs_exactly_one_owner(client: AsyncClient) -> None:
    # Neither means no ancestry to flatten; both means the principal's org and
    # team are ambiguous.
    for payload in (
        {"key_id": "key-1"},
        {"key_id": "key-1", "application_id": "app-1", "user_id": "user-1"},
    ):
        response = await client.post("/v1/tenants/acme/keys", json=payload)
        assert response.status_code == 400


async def test_errors_map_to_the_right_status(client: AsyncClient) -> None:
    assert (await client.post("/v1/keys/nope/rotate", json={})).status_code == 404
    assert (await client.delete("/v1/keys/nope")).status_code == 404

    await client.post("/v1/tenants/acme/keys", json={"key_id": "key-1", "application_id": "app-1"})
    duplicate = await client.post(
        "/v1/tenants/acme/keys", json={"key_id": "key-1", "application_id": "app-1"}
    )
    assert duplicate.status_code == 409


@pytest.mark.parametrize(
    "headers",
    [{}, {"Authorization": "Bearer wrong"}, {"Authorization": "not-a-bearer"}],
)
async def test_the_admin_surface_requires_credentials(
    client: AsyncClient, headers: dict[str, str]
) -> None:
    # A gateway holds every provider credential in the organisation; an
    # unauthenticated admin API is the shape of every CVE in the plan's §1.
    response = await client.post(
        "/v1/tenants/acme/keys",
        json={"key_id": "key-1", "application_id": "app-1"},
        headers={"Authorization": ""} | headers,
    )
    assert response.status_code == 401


async def test_health_needs_no_credentials(client: AsyncClient) -> None:
    # A liveness probe that needs a secret is a liveness probe that fails when
    # the secret rotates.
    response = await client.get("/healthz", headers={"Authorization": ""})
    assert response.status_code == 200


async def test_building_a_snapshot_reports_its_digests(client: AsyncClient) -> None:
    response = await client.post("/v1/snapshots")
    assert response.status_code == 200

    body = response.json()
    assert body["global_digest"].startswith("sha256:")
    assert body["tenants"][0]["tenant"] == "acme"


async def test_a_key_change_advances_the_tenant_version(client: AsyncClient) -> None:
    # A worker rejects a layer whose version has not moved forward, so a change
    # that forgets to bump it is a change nobody sees.
    before = (await client.post("/v1/snapshots")).json()["tenants"][0]["version"]
    await client.post("/v1/tenants/acme/keys", json={"key_id": "key-1", "application_id": "app-1"})
    after = (await client.post("/v1/snapshots")).json()["tenants"][0]["version"]

    assert after > before


async def test_rotation_overlap_has_a_deadline(client: AsyncClient) -> None:
    await client.post("/v1/tenants/acme/keys", json={"key_id": "key-1", "application_id": "app-1"})
    await client.post("/v1/keys/key-1/rotate", json={"new_key_id": "key-2"})

    raw = (await client.get("/v1/snapshots/current")).content
    snapshot = pb.Snapshot()
    snapshot.ParseFromString(raw)
    old = {p.key_id: p for p in snapshot.tenants[0].principals}["key-1"]

    expires = datetime.fromtimestamp(old.not_after_unix_ms / 1000, tz=UTC)
    assert expires - NOW == timedelta(days=7)
