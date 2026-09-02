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
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
from httpx import ASGITransport, AsyncClient
from sqlalchemy.ext.asyncio import AsyncEngine, AsyncSession

from model_gateway_control.api.app import AdminSettings, create_app
from model_gateway_control.db import models
from model_gateway_control.db.models import Base
from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.domain.component import manifest_from_dict
from model_gateway_control.domain.finetune import Spec as FineTuneSpec
from model_gateway_control.domain.identity import compute_key_lookup
from model_gateway_control.domain.scorecard import Scorecard
from model_gateway_control.domain.signing import (
    Policy,
    PublisherKey,
    TrustStore,
    sign,
)
from model_gateway_control.service.evaluator import Target
from model_gateway_control.service.finetune import Evaluators, Trainers
from model_gateway_control.service.trainer import Run
from model_gateway_control.wire import snapshot_pb2 as pb

DEFAULT_URL = "sqlite+aiosqlite:///:memory:"
PEPPER = b"an-admin-api-test-pepper-32-bytes!!"
TOKEN = "test-admin-token"
NOW = datetime(2026, 9, 1, 12, 0, 0, tzinfo=UTC)


@pytest_asyncio.fixture
async def engine() -> AsyncIterator[AsyncEngine]:
    """A freshly created schema, seeded with one tenant.

    Separate from ``client`` so a test can reach the same database directly —
    in-memory SQLite gives each engine its own database, so a second one built
    from the same URL would silently be a different world.
    """
    engine = create_engine(os.environ.get("GATEWAY_TEST_DATABASE_URL", DEFAULT_URL))
    async with engine.begin() as connection:
        await connection.run_sync(Base.metadata.drop_all)
        await connection.run_sync(Base.metadata.create_all)

    async with session_factory(engine)() as session:
        await _seed(session)

    yield engine
    await engine.dispose()


@pytest_asyncio.fixture
async def client(engine: AsyncEngine) -> AsyncIterator[AsyncClient]:
    """An API client over that schema."""
    app = create_app(
        AdminSettings(engine=engine, key_pepper=PEPPER, admin_token=TOKEN, now=lambda: NOW)
    )
    async with AsyncClient(
        transport=ASGITransport(app=app),
        base_url="http://admin",
        headers={"Authorization": f"Bearer {TOKEN}"},
    ) as client:
        yield client


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
    # never sees. Asserting the route simply does not exist is the only honest
    # way to state that — an earlier version of this asserted the response was
    # "not None", which is true of every response and tested nothing.
    readback = await client.get("/v1/keys/key-1")
    assert readback.status_code in (404, 405)


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
    revoked = await client.delete("/v1/keys/key-1")
    assert revoked.status_code == 204

    raw = (await client.get("/v1/snapshots/current")).content
    snapshot = pb.Snapshot()
    snapshot.ParseFromString(raw)
    assert len(snapshot.tenants[0].principals) == 0

    # Revoking twice is not an error; an agent retrying must not get a 404.
    again = await client.delete("/v1/keys/key-1")
    assert again.status_code == 204


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
    rotate_missing = await client.post("/v1/keys/nope/rotate", json={})
    assert rotate_missing.status_code == 404

    revoke_missing = await client.delete("/v1/keys/nope")
    assert revoke_missing.status_code == 404

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


async def test_rate_limits_reach_the_snapshot(client: AsyncClient) -> None:
    # A limit that the worker never sees is a limit that does not exist.
    await client.post(
        "/v1/tenants/acme/keys",
        json={
            "key_id": "key-1",
            "application_id": "app-1",
            "requests_per_minute": 600,
            "tokens_per_minute": 90000,
            "max_concurrent": 32,
        },
    )

    raw = (await client.get("/v1/snapshots/current")).content
    snapshot = pb.Snapshot()
    snapshot.ParseFromString(raw)
    principal = snapshot.tenants[0].principals[0]

    assert principal.limits.requests_per_minute == 600
    assert principal.limits.tokens_per_minute == 90000
    assert principal.limits.max_concurrent == 32
    # Also written at the superseded tag, so a worker built before RateLimit
    # existed still sees the concurrency limit during a rollout.
    assert principal.max_concurrent == 32


async def test_omitted_limits_mean_unlimited(client: AsyncClient) -> None:
    # Not capped at zero: adding a field must not be an outage for every key
    # that predates it.
    await client.post("/v1/tenants/acme/keys", json={"key_id": "key-1", "application_id": "app-1"})

    raw = (await client.get("/v1/snapshots/current")).content
    snapshot = pb.Snapshot()
    snapshot.ParseFromString(raw)
    limits = snapshot.tenants[0].principals[0].limits

    assert limits.requests_per_minute == 0
    assert limits.tokens_per_minute == 0
    assert limits.max_concurrent == 0


async def test_rotation_carries_limits_to_the_successor(client: AsyncClient) -> None:
    # A rotated key that silently gets different limits looks like a capacity
    # problem in the caller.
    await client.post(
        "/v1/tenants/acme/keys",
        json={"key_id": "key-1", "application_id": "app-1", "requests_per_minute": 120},
    )
    await client.post("/v1/keys/key-1/rotate", json={"new_key_id": "key-2"})

    raw = (await client.get("/v1/snapshots/current")).content
    snapshot = pb.Snapshot()
    snapshot.ParseFromString(raw)
    by_id = {p.key_id: p for p in snapshot.tenants[0].principals}

    assert by_id["key-2"].limits.requests_per_minute == 120


async def test_a_negative_limit_is_rejected(client: AsyncClient) -> None:
    response = await client.post(
        "/v1/tenants/acme/keys",
        json={"key_id": "key-1", "application_id": "app-1", "requests_per_minute": -1},
    )
    assert response.status_code == 422


# --- component registry -----------------------------------------------------

COMPONENT = {
    "name": "presidio",
    "version": "2.1.0",
    "port": "guardrail",
    "latency_budget_ms": 200,
    "failure_mode": "open",
    "execution": "sidecar",
    "capabilities": ["network"],
}


async def test_a_registered_component_is_pending_until_a_run_admits_it(
    client: AsyncClient,
) -> None:
    created = await client.post("/v1/components", json=COMPONENT)
    assert created.status_code == 201
    body = created.json()

    assert body["status"] == "pending"
    assert len(body["digest"]) == 64
    assert body["admission"] is None


async def test_recording_a_passing_run_activates_the_component(client: AsyncClient) -> None:
    digest = (await client.post("/v1/components", json=COMPONENT)).json()["digest"]

    admitted = await client.post(
        "/v1/components/presidio/2.1.0/admissions",
        json={
            "suite": "guardrail",
            "suite_version": "3",
            "manifest_digest": digest,
            "passed": True,
            "runner": "sandbox://ephemeral",
            "evidence_ref": "s3://runs/42",
        },
    )

    assert admitted.status_code == 200
    body = admitted.json()
    assert body["status"] == "active"
    assert body["admission"]["runner"] == "sandbox://ephemeral"
    assert body["admission"]["suite_version"] == "3"


async def test_a_run_against_a_different_manifest_is_rejected(client: AsyncClient) -> None:
    # The runner reports; it does not decide. A verdict that covers other bytes
    # would admit an artifact nothing tested.
    await client.post("/v1/components", json=COMPONENT)

    conflict = await client.post(
        "/v1/components/presidio/2.1.0/admissions",
        json={
            "suite": "guardrail",
            "suite_version": "3",
            "manifest_digest": "0" * 64,
            "passed": True,
            "runner": "sandbox://ephemeral",
        },
    )

    assert conflict.status_code == 409


async def test_republishing_a_version_is_a_conflict(client: AsyncClient) -> None:
    await client.post("/v1/components", json=COMPONENT)
    again = await client.post("/v1/components", json=COMPONENT)

    assert again.status_code == 409


async def test_a_manifest_that_could_never_be_bound_is_rejected_at_the_door(
    client: AsyncClient,
) -> None:
    # A request-path component with no declared budget, and an image pinned by
    # a floating tag. Both are refused before anything is stored.
    no_budget = await client.post("/v1/components", json=COMPONENT | {"latency_budget_ms": 0})
    assert no_budget.status_code == 400

    floating = await client.post(
        "/v1/components", json=COMPONENT | {"image": "ghcr.io/acme/presidio:latest"}
    )
    assert floating.status_code == 400


async def test_retiring_leaves_the_record_and_stops_binding(client: AsyncClient) -> None:
    await client.post("/v1/components", json=COMPONENT)
    retired = await client.delete("/v1/components/presidio/2.1.0")

    assert retired.status_code == 200
    assert retired.json()["status"] == "retired"
    # Not a deletion: what was once bindable stays answerable.
    assert (await client.get("/v1/components/presidio/2.1.0")).json()["status"] == "retired"


async def test_components_can_be_listed_by_port(client: AsyncClient) -> None:
    await client.post("/v1/components", json=COMPONENT)
    await client.post(
        "/v1/components",
        json={"name": "llamafactory", "version": "0.9.0", "port": "trainer"},
    )

    guardrails = (await client.get("/v1/components", params={"port": "guardrail"})).json()
    assert [c["name"] for c in guardrails["components"]] == ["presidio"]

    every = (await client.get("/v1/components")).json()
    assert {c["name"] for c in every["components"]} == {"presidio", "llamafactory"}


async def test_the_registry_endpoints_require_the_admin_token(client: AsyncClient) -> None:
    # The registry decides what code the fleet will run. An unauthenticated
    # write to it is the whole "nice admin UI" failure in one request.
    for method, path in (
        ("POST", "/v1/components"),
        ("GET", "/v1/components"),
        ("DELETE", "/v1/components/presidio/2.1.0"),
        ("POST", "/v1/components/presidio/2.1.0/admissions"),
    ):
        response = await client.request(method, path, json=COMPONENT, headers={"Authorization": ""})
        assert response.status_code == 401, f"{method} {path}"


async def test_a_snapshot_cannot_bind_a_component_that_was_never_admitted(
    client: AsyncClient, engine: AsyncEngine
) -> None:
    # The end of the chain: registration alone must not reach a worker. This is
    # the same refusal the builder makes, seen through the API an operator uses.
    await client.post("/v1/components", json=COMPONENT)
    async with session_factory(engine)() as session:
        session.add(models.PluginBinding(tenant_id=None, port="guardrail", component="presidio"))
        await session.commit()

    refused = await client.post("/v1/snapshots")

    assert refused.status_code == 400
    assert "presidio" in refused.json()["error"]["message"]


# --- publisher signatures ---------------------------------------------------


@pytest.fixture
async def signing_client(engine: AsyncEngine) -> AsyncIterator[tuple[AsyncClient, object]]:
    """A client whose control plane requires a signature from one known key."""
    private = Ed25519PrivateKey.generate()
    public = private.public_key().public_bytes(encoding=Encoding.Raw, format=PublicFormat.Raw)
    key = PublisherKey(key_id="acme-2026", publisher="ACME", public_key=public)

    app = create_app(
        AdminSettings(
            engine=engine,
            key_pepper=PEPPER,
            admin_token=TOKEN,
            trust=TrustStore(keys=(key,), policy=Policy.REQUIRED),
            now=lambda: NOW,
        )
    )
    async with AsyncClient(
        transport=ASGITransport(app=app),
        base_url="http://admin",
        headers={"Authorization": f"Bearer {TOKEN}"},
    ) as client:
        yield client, private


def _manifest_body(**overrides: object) -> dict[str, object]:
    body: dict[str, object] = {
        "name": "acme-guard",
        "version": "1.0.0",
        "port": "guardrail",
        "latency_budget_ms": 50,
        "execution": "sidecar",
        "image": "ghcr.io/acme/guard@sha256:" + "0" * 64,
    }
    body.update(overrides)
    return body


async def test_a_signed_registration_records_who_vouched_for_it(
    signing_client: tuple[AsyncClient, Ed25519PrivateKey],
) -> None:
    client, private = signing_client
    body = _manifest_body()
    digest = manifest_from_dict(dict(body)).digest()
    signature = sign(digest, private, "acme-2026")

    created = await client.post(
        "/v1/components",
        json=body | {"signing_key_id": "acme-2026", "signature": signature.encoded()},
    )
    assert created.status_code == 201, created.text

    fetched = await client.get("/v1/components/acme-guard/1.0.0")
    assert fetched.json()["signing_key_id"] == "acme-2026"
    # The signature comes back so anyone can re-check it, rather than having to
    # take the control plane's word that it was checked.
    assert fetched.json()["signature"] == signature.encoded()


async def test_an_unsigned_registration_is_refused_when_policy_requires_one(
    signing_client: tuple[AsyncClient, Ed25519PrivateKey],
) -> None:
    client, _ = signing_client

    response = await client.post("/v1/components", json=_manifest_body())

    assert response.status_code == 403
    assert "signature" in response.text


async def test_a_signature_over_a_different_manifest_is_refused(
    signing_client: tuple[AsyncClient, Ed25519PrivateKey],
) -> None:
    # The publisher signed version 1.0.0 and submitted 1.0.1. Catching it here
    # is the difference between a signature and a decoration.
    client, private = signing_client
    signed = manifest_from_dict(_manifest_body())
    signature = sign(signed.digest(), private, "acme-2026")

    response = await client.post(
        "/v1/components",
        json=_manifest_body(version="1.0.1")
        | {"signing_key_id": "acme-2026", "signature": signature.encoded()},
    )

    assert response.status_code == 403
    assert "does not match this manifest" in response.text


async def test_registration_still_works_with_no_signing_configured(
    client: AsyncClient,
) -> None:
    # The default deployment must keep working, or turning signing on becomes a
    # prerequisite for using the registry at all.
    response = await client.post("/v1/components", json=_manifest_body())

    assert response.status_code == 201
    assert response.json()["signing_key_id"] == ""


# --- fine-tuning ------------------------------------------------------------


class StubTrainer:
    """A trainer the API can resolve. It is never called from these tests.

    Typed against the real port rather than against ``object``: a stub that
    does not satisfy TrainerPort would let the API accept a registration the
    reconciler could never act on.
    """

    def name(self) -> str:
        return "llamafactory-lora"

    async def submit(self, job_name: str, spec: FineTuneSpec, idempotency_key: str) -> Run:
        raise NotImplementedError

    async def poll(self, external_id: str) -> Run:
        raise NotImplementedError

    async def cancel(self, external_id: str) -> None:
        raise NotImplementedError


class StubEvaluator:
    """A suite the API can resolve. It is never run from these tests."""

    def name(self) -> str:
        return "triage-regression-v2"

    def version(self) -> str:
        return "1.0.0"

    async def run(self, target: Target) -> Scorecard:
        raise NotImplementedError


@pytest.fixture
async def training_client(engine: AsyncEngine) -> AsyncIterator[AsyncClient]:
    app = create_app(
        AdminSettings(
            engine=engine,
            key_pepper=PEPPER,
            admin_token=TOKEN,
            trainers=Trainers((StubTrainer(),)),
            evaluators=Evaluators((StubEvaluator(),)),
            now=lambda: NOW,
        )
    )
    async with AsyncClient(
        transport=ASGITransport(app=app),
        base_url="http://admin",
        headers={"Authorization": f"Bearer {TOKEN}"},
    ) as client:
        yield client


def _job_body(**overrides: object) -> dict[str, object]:
    body: dict[str, object] = {
        "name": "support-triage-v3",
        "tenant": "acme",
        "base_model": "llama-3.3-70b",
        "trainer": "llamafactory-lora",
        "trainer_version": "1.0.0",
        "dataset_uri": "s3://acme-training/triage-v3.jsonl",
        "dataset_checksum": "sha256:" + "a" * 64,
        "dataset_rows": 48210,
        "dataset_schema_version": "chatml-v1",
        "budget_ref": "acme/training-q3",
    }
    body.update(overrides)
    return body


async def test_a_submitted_job_starts_pending_and_can_be_polled(
    training_client: AsyncClient,
) -> None:
    # The shape an agent drives: POST a spec, then poll until the phase
    # settles, rather than orchestrating the steps itself.
    created = await training_client.post("/v1/finetune/jobs", json=_job_body())
    assert created.status_code == 201, created.text
    assert created.json()["status"]["phase"] == "pending"

    fetched = await training_client.get("/v1/finetune/jobs/acme/support-triage-v3")
    assert fetched.status_code == 200
    assert fetched.json()["spec"]["dataset"]["rows"] == 48210


async def test_a_client_cannot_set_a_job_status(training_client: AsyncClient) -> None:
    # A client that could set the phase could mark a job trained without
    # anything having been trained.
    response = await training_client.post(
        "/v1/finetune/jobs",
        json=_job_body() | {"status": {"phase": "trained"}, "phase": "trained"},
    )

    assert response.status_code == 201
    assert response.json()["status"]["phase"] == "pending"
    assert response.json()["status"]["artifact_ref"] == ""


async def test_a_job_naming_an_unconfigured_trainer_is_refused(
    training_client: AsyncClient,
) -> None:
    # At submission, while someone is watching, rather than in a reconciler log
    # nobody is reading.
    response = await training_client.post(
        "/v1/finetune/jobs", json=_job_body(trainer="a-trainer-nobody-configured")
    )

    assert response.status_code == 404
    assert "no trainer named" in response.text


async def test_a_dataset_without_a_checksum_is_refused(training_client: AsyncClient) -> None:
    # Without one, the data behind that URI can change after the job runs.
    response = await training_client.post(
        "/v1/finetune/jobs", json=_job_body(dataset_checksum="latest")
    )

    assert response.status_code == 400
    assert "checksum" in response.text


async def test_resubmitting_the_same_job_name_is_a_conflict(
    training_client: AsyncClient,
) -> None:
    assert (await training_client.post("/v1/finetune/jobs", json=_job_body())).status_code == 201

    response = await training_client.post("/v1/finetune/jobs", json=_job_body())

    assert response.status_code == 409


async def test_cancelling_a_pending_job_settles_it(training_client: AsyncClient) -> None:
    await training_client.post("/v1/finetune/jobs", json=_job_body())

    cancelled = await training_client.post("/v1/finetune/jobs/acme/support-triage-v3/cancel")

    assert cancelled.status_code == 200
    assert cancelled.json()["status"]["phase"] == "cancelled"


async def test_a_gate_is_recorded_on_the_spec_and_comes_back(
    training_client: AsyncClient,
) -> None:
    # The bar is fixed at submission, so lowering it later cannot retroactively
    # promote something that already failed it. That only holds if it is stored
    # with the job rather than read from configuration when the gate runs.
    created = await training_client.post(
        "/v1/finetune/jobs",
        json=_job_body(
            eval_suite="triage-regression-v2",
            min_score=8_700,
            must_not_regress=["latency_p95", "refusal_rate"],
        ),
    )

    assert created.status_code == 201, created.text
    gate = created.json()["spec"]["promotion_gate"]
    assert gate["min_score"] == 8_700
    assert gate["must_not_regress"] == ["latency_p95", "refusal_rate"]
    # Nothing has measured it yet.
    assert created.json()["status"]["scorecard"] is None


async def test_a_score_outside_the_basis_point_range_is_refused(
    training_client: AsyncClient,
) -> None:
    # A gate given 0.87 rather than 8700 would pass everything, and a gate
    # given 87 would pass almost everything. Neither fails loudly at runtime.
    response = await training_client.post("/v1/finetune/jobs", json=_job_body(min_score=20_000))

    assert response.status_code == 422
