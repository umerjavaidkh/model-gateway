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
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Annotated, Any

from fastapi import Depends, FastAPI, Header, HTTPException, Query, Request, Response, status
from fastapi.responses import HTMLResponse
from pydantic import BaseModel, Field
from sqlalchemy.ext.asyncio import AsyncEngine, AsyncSession

from model_gateway_control.api import idempotency
from model_gateway_control.api.dashboard import DEFAULT_GATEWAY_URL, render_dashboard
from model_gateway_control.db.repository import Repository
from model_gateway_control.db.session import session_factory
from model_gateway_control.domain.budget import BudgetScope
from model_gateway_control.domain.catalog import Capability, TrustTier
from model_gateway_control.domain.component import (
    Admission,
    Component,
    Execution,
    FailureMode,
    Manifest,
    Port,
)
from model_gateway_control.domain.finetune import (
    DEFAULT_CANARY_STEPS,
    DatasetRef,
    FineTuneJob,
)
from model_gateway_control.domain.finetune import Spec as FineTuneSpec
from model_gateway_control.domain.identity import RateLimit
from model_gateway_control.domain.policy import PolicyBundle, PolicyEffect, PolicyRule
from model_gateway_control.domain.scorecard import BASIS_POINTS, PromotionGate, Scorecard
from model_gateway_control.domain.signing import Signature, TrustStore
from model_gateway_control.errors import (
    ConflictError,
    ForbiddenError,
    GatewayError,
    InvalidRequestError,
    NotFoundError,
)
from model_gateway_control.service.finetune import Evaluators, FineTuneService, Trainers
from model_gateway_control.service.keys import KeyService
from model_gateway_control.service.policy import PolicyService
from model_gateway_control.service.provisioning import DeploymentSpec, ProvisioningService
from model_gateway_control.service.registry import RegistryService
from model_gateway_control.service.requests import RequestLog, RequestRecord
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
    #: Publisher keys and how strictly signatures are enforced. Loaded from
    #: configuration rather than the database, because a trust root the
    #: database can change is not one.
    trust: TrustStore = field(default_factory=TrustStore)
    #: Trainer backends this control plane can submit fine-tune jobs to. Empty
    #: means jobs can still be submitted and will sit in PENDING, which is the
    #: honest behaviour for a deployment that has not configured one — but the
    #: API refuses a job naming a trainer it does not have, so the mistake
    #: surfaces at submission rather than in a reconciler log.
    trainers: Trainers = field(default_factory=Trainers)
    #: Eval suites this control plane can run. A job naming one it does not
    #: have is refused at submission, because a job whose gate can never run
    #: would train at full cost and then stall.
    evaluators: Evaluators = field(default_factory=Evaluators)
    #: Where the console's chat tab posts by default. The data plane is a
    #: different process on a different host in every deployment that is
    #: not a laptop, so it cannot be a constant in the page.
    gateway_url: str = DEFAULT_GATEWAY_URL
    now: Callable[[], datetime] | None = None


class IssueKeyRequest(BaseModel):
    """Issue a key for exactly one application or user."""

    key_id: str = Field(min_length=1, max_length=64)
    application_id: str | None = None
    user_id: str | None = None
    models_allow_all: bool = False
    min_trust_tier: str = "EXTERNAL"
    # Zero means unlimited for that dimension, matching the domain default, so
    # omitting them keeps the previous behaviour rather than capping at nothing.
    requests_per_minute: int = Field(default=0, ge=0)
    tokens_per_minute: int = Field(default=0, ge=0)
    max_concurrent: int = Field(default=0, ge=0)


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


class RegisterComponentRequest(BaseModel):
    """A manifest submitted to the registry.

    Mirrors ``domain.component.Manifest``. It is a separate type because this
    one is a wire contract with a stability obligation, and the domain type is
    free to be refactored.
    """

    name: str = Field(min_length=3, max_length=64)
    version: str = Field(min_length=1, max_length=64)
    port: Port
    config_schema: str = "{}"
    latency_budget_ms: int = Field(default=0, ge=0)
    failure_mode: FailureMode = FailureMode.CLOSED
    execution: Execution = Execution.SIDECAR
    capabilities: list[str] = Field(default_factory=list)
    image: str = ""
    module: str = ""
    #: The publisher's signature over this manifest's digest, base64, and the
    #: key that produced it. Optional here and required by policy, so a
    #: deployment can turn signing on after its publishers have keys rather
    #: than before.
    signing_key_id: str = ""
    signature: str = ""

    def to_signature(self) -> Signature | None:
        """The submitted signature, or None when the request carries none.

        Half a signature is treated as none rather than as an error: it proves
        nothing either way, and the policy check that follows is what decides
        whether proving nothing is allowed.
        """
        if not self.signing_key_id or not self.signature:
            return None
        return Signature.decode(self.signing_key_id, self.signature)

    def to_manifest(self) -> Manifest:
        return Manifest(
            name=self.name,
            version=self.version,
            port=self.port,
            config_schema=self.config_schema,
            latency_budget_ms=self.latency_budget_ms,
            failure_mode=self.failure_mode,
            execution=self.execution,
            capabilities=tuple(self.capabilities),
            image=self.image,
            module=self.module,
        )


class RecordAdmissionRequest(BaseModel):
    """The verdict of a contract-suite run that happened somewhere else."""

    suite: Port
    suite_version: str = Field(min_length=1, max_length=64)
    #: The manifest the suite actually examined. Sent rather than assumed, so a
    #: run against a stale copy is rejected instead of silently admitting one.
    manifest_digest: str = Field(min_length=64, max_length=64)
    passed: bool
    runner: str = Field(min_length=1, max_length=255)
    evidence_ref: str = ""

    def to_admission(self) -> Admission:
        return Admission(
            suite=self.suite,
            suite_version=self.suite_version,
            manifest_digest=self.manifest_digest,
            passed=self.passed,
            runner=self.runner,
            evidence_ref=self.evidence_ref,
        )


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
            limits=RateLimit(
                requests_per_minute=body.requests_per_minute,
                tokens_per_minute=body.tokens_per_minute,
                max_concurrent=body.max_concurrent,
            ),
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

    # ---- component registry -------------------------------------------------
    #
    # Registration and admission are separate endpoints because they are
    # separate authorities. Submitting a manifest is a publisher's act;
    # admitting one is the platform's, and it requires a contract-suite run
    # that this process must not perform itself.

    @app.post("/v1/components", status_code=status.HTTP_201_CREATED, dependencies=[Authorized])
    async def register_component(body: RegisterComponentRequest, session: Session) -> Response:
        component = await RegistryService(session, trust=settings.trust).register(
            body.to_manifest(), body.to_signature()
        )
        await session.commit()
        return _json_response(status.HTTP_201_CREATED, _component_json(component))

    # --- fine-tuning ------------------------------------------------------
    #
    # Declarative, because an agent drives it: a client POSTs a spec and polls
    # until the phase settles, rather than orchestrating submit, poll and
    # commit and trying to recover when step three fails.

    @app.post("/v1/finetune/jobs", status_code=status.HTTP_201_CREATED, dependencies=[Authorized])
    async def submit_finetune_job(body: SubmitFineTuneJobRequest, session: Session) -> Response:
        job = await FineTuneService(session, settings.trainers, settings.evaluators).submit(
            body.to_job()
        )
        await session.commit()
        return _json_response(status.HTTP_201_CREATED, _job_json(job))

    @app.get("/v1/finetune/jobs", dependencies=[Authorized])
    async def list_finetune_jobs(session: Session, tenant: str | None = None) -> Response:
        jobs = await FineTuneService(session, settings.trainers, settings.evaluators).list(tenant)
        return _json_response(status.HTTP_200_OK, {"jobs": [_job_json(j) for j in jobs]})

    @app.get("/v1/finetune/jobs/{tenant}/{name}", dependencies=[Authorized])
    async def get_finetune_job(tenant: str, name: str, session: Session) -> Response:
        job = await FineTuneService(session, settings.trainers, settings.evaluators).get(
            tenant, name
        )
        return _json_response(status.HTTP_200_OK, _job_json(job))

    # Rollout steps are operator decisions rather than a timer's. Without a
    # health signal to advance on, a rollout that promoted itself would promote
    # a bad adapter just as reliably as a good one — and a fine-tuned
    # regression is silent, so nothing would notice.

    @app.post("/v1/finetune/jobs/{tenant}/{name}/rollout", dependencies=[Authorized])
    async def start_rollout(tenant: str, name: str, session: Session) -> Response:
        service = FineTuneService(session, settings.trainers, settings.evaluators)
        job = await service.start_rollout(tenant, name)
        await session.commit()
        return _json_response(status.HTTP_200_OK, _job_json(job))

    @app.post("/v1/finetune/jobs/{tenant}/{name}/rollout/advance", dependencies=[Authorized])
    async def advance_rollout(tenant: str, name: str, session: Session) -> Response:
        service = FineTuneService(session, settings.trainers, settings.evaluators)
        job = await service.advance_rollout(tenant, name)
        await session.commit()
        return _json_response(status.HTTP_200_OK, _job_json(job))

    @app.post("/v1/finetune/jobs/{tenant}/{name}/rollout/abort", dependencies=[Authorized])
    async def abort_rollout(tenant: str, name: str, session: Session) -> Response:
        service = FineTuneService(session, settings.trainers, settings.evaluators)
        job = await service.abort_rollout(tenant, name)
        await session.commit()
        return _json_response(status.HTTP_200_OK, _job_json(job))

    @app.post("/v1/finetune/jobs/{tenant}/{name}/cancel", dependencies=[Authorized])
    async def cancel_finetune_job(tenant: str, name: str, session: Session) -> Response:
        job = await FineTuneService(session, settings.trainers, settings.evaluators).cancel(
            tenant, name
        )
        await session.commit()
        return _json_response(status.HTTP_200_OK, _job_json(job))

    # --- provisioning -----------------------------------------------------
    #
    # PUT rather than POST for everything but a key: the caller states the
    # configuration it wants and the gateway makes it so, whether or not it
    # already existed. A deploy script or a compliance engine restating its
    # position should not have to work out what it said last time, and a retry
    # after a crash must not be a second tenant.

    @app.put("/v1/tenants/{tenant}", dependencies=[Authorized])
    async def put_tenant(tenant: str, body: PutTenantRequest, session: Session) -> Response:
        row = await ProvisioningService(session).ensure_tenant(
            tenant, tier=body.tier, min_trust_tier=body.min_trust_tier
        )
        await session.commit()
        return _json_response(
            status.HTTP_200_OK,
            {"id": row.id, "tier": row.tier, "version": row.version, "key_prefix": row.id},
        )

    @app.put("/v1/deployments/{deployment_id}", dependencies=[Authorized])
    async def put_deployment(
        deployment_id: str, body: PutDeploymentRequest, session: Session
    ) -> Response:
        row = await ProvisioningService(session).ensure_deployment(body.to_spec(deployment_id))
        await session.commit()
        return _json_response(status.HTTP_200_OK, _deployment_json(row))

    @app.get("/v1/deployments", dependencies=[Authorized])
    async def list_deployments(session: Session) -> Response:
        rows = await ProvisioningService(session).list_deployments()
        return _json_response(
            status.HTTP_200_OK, {"deployments": [_deployment_json(r) for r in rows]}
        )

    @app.delete(
        "/v1/deployments/{deployment_id}",
        status_code=status.HTTP_204_NO_CONTENT,
        dependencies=[Authorized],
    )
    async def delete_deployment(deployment_id: str, session: Session) -> Response:
        await ProvisioningService(session).remove_deployment(deployment_id)
        await session.commit()
        return Response(status_code=status.HTTP_204_NO_CONTENT)

    @app.put("/v1/aliases/{name}", dependencies=[Authorized])
    async def put_alias(name: str, body: PutAliasRequest, session: Session) -> Response:
        await ProvisioningService(session).ensure_alias(name, body.targets)
        await session.commit()
        return _json_response(status.HTTP_200_OK, {"name": name, "targets": body.targets})

    @app.put("/v1/budgets/{budget_id}", dependencies=[Authorized])
    async def put_budget(budget_id: str, body: PutBudgetRequest, session: Session) -> Response:
        row = await ProvisioningService(session).ensure_budget(
            budget_id,
            tenant_id=body.tenant,
            limit_micro_usd=body.limit_micro_usd,
            scope=body.scope,
            hard=body.hard,
            headroom_basis_points=body.headroom_basis_points,
        )
        await session.commit()
        return _json_response(
            status.HTTP_200_OK,
            {
                "id": row.id,
                "tenant": row.tenant_id,
                "limit_micro_usd": row.limit_micro_usd,
                "spent_micro_usd": row.spent_micro_usd,
                "hard": row.hard,
            },
        )

    # --- what traffic did -------------------------------------------------
    #
    # Read-only, and separate from the metrics endpoint on purpose: Prometheus
    # answers "how many, how fast" across everything, and these answer "what
    # happened to this one" for whoever has to explain it.

    @app.get("/v1/requests", dependencies=[Authorized])
    async def list_requests(
        session: Session,
        limit: int = 100,
        failed: bool = False,
        tenant: str | None = None,
        shadow: bool = False,
    ) -> Response:
        records = await RequestLog(session).recent(
            limit=limit, failed_only=failed, tenant=tenant, include_shadow=shadow
        )
        return _json_response(status.HTTP_200_OK, {"requests": [_request_json(r) for r in records]})

    @app.get("/v1/requests/summary", dependencies=[Authorized])
    async def request_summary(session: Session) -> Response:
        return _json_response(
            status.HTTP_200_OK, {"failures": await RequestLog(session).failure_summary()}
        )

    @app.get("/v1/requests/{request_id}", dependencies=[Authorized])
    async def get_request(request_id: str, session: Session) -> Response:
        record = await RequestLog(session).get(request_id)
        return _json_response(status.HTTP_200_OK, _request_json(record))

    @app.get("/dashboard", response_class=HTMLResponse, dependencies=[])
    async def dashboard() -> HTMLResponse:
        """A page for watching traffic and for generating some.

        Served from the admin API rather than as its own deployable: it is a
        read-only view over data this process already has, and a second
        container with its own build and its own CVE surface would be a lot of
        machinery for one page.

        Unauthenticated *here* because the page holds no data — every fetch it
        makes carries the token the operator pastes in, and is authorised like
        any other call. Shipping the token inside the page would put it in
        every browser cache that ever loaded it.
        """
        return HTMLResponse(render_dashboard(settings.gateway_url))

    # --- policy -----------------------------------------------------------
    #
    # The gateway evaluates policy; it does not decide what it should be. This
    # is where an external authority — a compliance engine, an operator, an
    # agent — publishes that decision. Workers compile it into their next
    # snapshot and evaluate it locally, so a rule costs nothing per request and
    # the authority being unreachable freezes policy rather than stopping
    # traffic.

    @app.put("/v1/policy", dependencies=[Authorized])
    async def put_fleet_policy(body: PutPolicyRequest, session: Session) -> Response:
        bundle = await PolicyService(session).replace(None, [r.to_rule() for r in body.rules])
        await session.commit()
        return _json_response(status.HTTP_200_OK, _policy_json(bundle))

    @app.get("/v1/policy", dependencies=[Authorized])
    async def get_fleet_policy(session: Session) -> Response:
        return _json_response(
            status.HTTP_200_OK, _policy_json(await PolicyService(session).get(None))
        )

    @app.put("/v1/tenants/{tenant}/policy", dependencies=[Authorized])
    async def put_tenant_policy(tenant: str, body: PutPolicyRequest, session: Session) -> Response:
        bundle = await PolicyService(session).replace(tenant, [r.to_rule() for r in body.rules])
        await session.commit()
        return _json_response(status.HTTP_200_OK, _policy_json(bundle))

    @app.get("/v1/tenants/{tenant}/policy", dependencies=[Authorized])
    async def get_tenant_policy(tenant: str, session: Session) -> Response:
        bundle = await PolicyService(session).get(tenant)
        return _json_response(status.HTTP_200_OK, _policy_json(bundle))

    @app.get("/v1/components", dependencies=[Authorized])
    async def list_components(session: Session, port: Port | None = None) -> Response:
        components = await RegistryService(session).list(port)
        return _json_response(
            status.HTTP_200_OK, {"components": [_component_json(c) for c in components]}
        )

    @app.get("/v1/components/{name}/{version}", dependencies=[Authorized])
    async def get_component(name: str, version: str, session: Session) -> Response:
        component = await RegistryService(session).get(name, version)
        return _json_response(status.HTTP_200_OK, _component_json(component))

    @app.post("/v1/components/{name}/{version}/admissions", dependencies=[Authorized])
    async def record_admission(
        name: str, version: str, body: RecordAdmissionRequest, session: Session
    ) -> Response:
        """Record the verdict of a contract-suite run performed elsewhere.

        The runner reports; it does not decide. The service checks the verdict
        binds to the manifest that is actually registered, so a run against a
        different artifact cannot admit this one.
        """
        component = await RegistryService(session).record_admission(
            name, version, body.to_admission()
        )
        await session.commit()
        return _json_response(status.HTTP_200_OK, _component_json(component))

    @app.delete("/v1/components/{name}/{version}", dependencies=[Authorized])
    async def retire_component(name: str, version: str, session: Session) -> Response:
        """Withdraw a component from future snapshots.

        Not a deletion: existing snapshots that name it stay valid, and the
        record of what was once bindable is what an audit needs.
        """
        component = await RegistryService(session).retire(name, version)
        await session.commit()
        return _json_response(status.HTTP_200_OK, _component_json(component))

    @app.post("/v1/snapshots", dependencies=[Authorized])
    async def build(session: Session) -> Response:
        """Compile the current configuration and report what it produced.

        Returns the digests rather than the bytes so that a caller can tell
        whether anything changed without transferring a snapshot. The bytes are
        served separately.
        """
        repo = Repository(session)
        snapshot = build_snapshot(
            await repo.load_fleet(), await repo.load_tenants(), now(), settings.trust
        )
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
        snapshot = build_snapshot(
            await repo.load_fleet(), await repo.load_tenants(), now(), settings.trust
        )
        return Response(
            content=snapshot.SerializeToString(deterministic=True),
            media_type="application/x-protobuf",
            headers={"X-Snapshot-Digest": snapshot.global_layer.version.digest},
        )

    return app


class PutTenantRequest(BaseModel):
    """A tenant, and the hierarchy a key hangs off."""

    tier: str = "standard"
    min_trust_tier: TrustTier = TrustTier.EXTERNAL


class PutDeploymentRequest(BaseModel):
    """Somewhere a model can be served from."""

    base_model: str = Field(min_length=1, max_length=255)
    provider: str = Field(min_length=1, max_length=64)
    endpoint: str = Field(min_length=1, max_length=1024)
    trust_tier: TrustTier
    adapter_id: str = ""
    region: str = ""
    credential_ref: str = ""
    weight: int = Field(default=100, ge=0, le=100)
    input_cost_micro_usd: int = Field(default=0, ge=0)
    output_cost_micro_usd: int = Field(default=0, ge=0)
    cached_input_cost_micro_usd: int = Field(default=0, ge=0)
    cache_write_cost_micro_usd: int = Field(default=0, ge=0)
    capabilities: list[Capability] = Field(default_factory=list)

    def to_spec(self, deployment_id: str) -> DeploymentSpec:
        return DeploymentSpec(
            id=deployment_id,
            base_model=self.base_model,
            provider=self.provider,
            endpoint=self.endpoint,
            trust_tier=self.trust_tier,
            adapter_id=self.adapter_id,
            region=self.region,
            credential_ref=self.credential_ref,
            weight=self.weight,
            input_cost_micro_usd=self.input_cost_micro_usd,
            output_cost_micro_usd=self.output_cost_micro_usd,
            cached_input_cost_micro_usd=self.cached_input_cost_micro_usd,
            cache_write_cost_micro_usd=self.cache_write_cost_micro_usd,
            capabilities=tuple(self.capabilities),
        )


class PutAliasRequest(BaseModel):
    """A friendly name for one or more base models, in preference order."""

    targets: list[str] = Field(min_length=1)


class PutBudgetRequest(BaseModel):
    """A spending limit. Spend itself is never set here."""

    tenant: str = Field(min_length=1, max_length=64)
    limit_micro_usd: int = Field(ge=0)
    scope: BudgetScope = BudgetScope.ORG
    hard: bool = True
    headroom_basis_points: int = Field(default=500, ge=0, le=BASIS_POINTS)


class PolicyRuleRequest(BaseModel):
    """One rule, as an external authority publishes it."""

    id: str = Field(min_length=1, max_length=64)
    effect: PolicyEffect
    models: list[str] = Field(default_factory=list)
    endpoints: list[str] = Field(default_factory=list)
    roles: list[str] = Field(default_factory=list)
    regions: list[str] = Field(default_factory=list)
    source_cidrs: list[str] = Field(default_factory=list)
    max_payload_bytes: int = Field(default=0, ge=0)
    data_class: str = ""
    min_trust_tier: TrustTier = TrustTier.UNSET
    #: Returned to the caller on a denial, so it must be safe to disclose.
    reason: str = ""

    def to_rule(self) -> PolicyRule:
        return PolicyRule(
            id=self.id,
            effect=self.effect,
            models=tuple(self.models),
            endpoints=tuple(self.endpoints),
            roles=tuple(self.roles),
            regions=tuple(self.regions),
            source_cidrs=tuple(self.source_cidrs),
            max_payload_bytes=self.max_payload_bytes,
            data_class=self.data_class,
            min_trust_tier=self.min_trust_tier,
            reason=self.reason,
        )


class PutPolicyRequest(BaseModel):
    """A whole rule set, in evaluation order.

    Whole rather than a patch: an authority restating its current position
    should be able to send that position without first working out what it said
    last time. It also makes a retry free, which matters when the publisher is
    a program that may crash mid-publish.
    """

    rules: list[PolicyRuleRequest] = Field(default_factory=list)


def _request_json(record: RequestRecord) -> dict[str, Any]:
    return {
        "request_id": record.request_id,
        "occurred_at": record.occurred_at.isoformat(),
        "tenant": record.tenant,
        "key_id": record.key_id,
        "deployment": record.deployment,
        "base_model": record.base_model,
        "adapter_id": record.adapter_id,
        "provider": record.provider,
        "stream": record.stream,
        "shadow": record.shadow,
        "outcome": record.outcome,
        # Which stage ended it. The final code says what went wrong; this says
        # where, which is what decides who looks at it next.
        "failed_at": record.failed_at,
        "latency_ms": record.latency_ms,
        "time_to_first_byte_ms": record.time_to_first_byte_ms,
        "input_tokens": record.input_tokens,
        "output_tokens": record.output_tokens,
        "cost_micro_usd": record.cost_micro_usd,
        "price_micro_usd": record.price_micro_usd,
        "snapshot_version": record.snapshot_version,
        "stages": [
            {"name": s.name, "duration_ms": s.duration_ms, "outcome": s.outcome}
            for s in record.stages
        ],
    }


def _deployment_json(row: Any) -> dict[str, Any]:
    return {
        "id": row.id,
        "base_model": row.base_model,
        "adapter_id": row.adapter_id,
        "provider": row.provider,
        "endpoint": row.endpoint,
        "region": row.region,
        "trust_tier": row.trust_tier,
        "credential_ref": row.credential_ref,
        "weight": row.weight,
        "capabilities": sorted(c.name for c in row.capabilities),
    }


def _policy_json(bundle: PolicyBundle) -> dict[str, Any]:
    return {
        "id": bundle.id,
        # Order is the whole of the conflict resolution — first match wins — so
        # it is returned as a list and never as a set.
        "rules": [
            {
                "id": rule.id,
                "effect": str(rule.effect),
                "models": list(rule.models),
                "endpoints": list(rule.endpoints),
                "roles": list(rule.roles),
                "regions": list(rule.regions),
                "source_cidrs": list(rule.source_cidrs),
                "max_payload_bytes": rule.max_payload_bytes,
                "data_class": rule.data_class,
                "min_trust_tier": int(rule.min_trust_tier),
                "reason": rule.reason,
            }
            for rule in bundle.rules
        ],
    }


class SubmitFineTuneJobRequest(BaseModel):
    """A fine-tune job as an operator or an agent submits it.

    Spec only. The status is the reconciler's, and a client that could set it
    could mark a job trained without anything having been trained.
    """

    name: str = Field(min_length=3, max_length=64)
    tenant: str = Field(min_length=1, max_length=64)
    base_model: str = Field(min_length=1, max_length=128)
    trainer: str = Field(min_length=1, max_length=64)
    trainer_version: str = Field(min_length=1, max_length=64)
    dataset_uri: str = Field(min_length=1, max_length=1024)
    dataset_checksum: str = Field(min_length=1, max_length=128)
    dataset_rows: int = Field(gt=0)
    dataset_schema_version: str = Field(min_length=1, max_length=64)
    hyperparameters: dict[str, str] = Field(default_factory=dict)
    budget_ref: str = ""
    eval_suite: str = ""
    #: The bar the suite must clear, in basis points out of 10,000. Integers
    #: rather than a fraction, so 0.87 against 0.8699999999 is not a coin toss
    #: decided by the last bits of a float.
    min_score: int = Field(default=0, ge=0, le=BASIS_POINTS)
    must_not_regress: list[str] = Field(default_factory=list)
    #: Traffic shares to walk through once the gate is cleared. A fine-tuned
    #: regression is silent, so the adapter climbs rather than being switched on.
    canary_steps: list[int] = Field(default_factory=lambda: list(DEFAULT_CANARY_STEPS))

    def to_job(self) -> FineTuneJob:
        return FineTuneJob(
            name=self.name,
            # Generated here, never accepted from the caller: two jobs sharing
            # a key would get the same run back from an idempotent trainer, and
            # the second would silently adopt the first's training.
            idempotency_key=FineTuneJob.new_key(),
            spec=FineTuneSpec(
                tenant=self.tenant,
                base_model=self.base_model,
                trainer=self.trainer,
                trainer_version=self.trainer_version,
                dataset=DatasetRef(
                    uri=self.dataset_uri,
                    checksum=self.dataset_checksum,
                    rows=self.dataset_rows,
                    schema_version=self.dataset_schema_version,
                ),
                hyperparameters=self.hyperparameters,
                budget_ref=self.budget_ref,
                eval_suite=self.eval_suite,
                canary_steps=tuple(self.canary_steps),
                promotion_gate=PromotionGate(
                    min_score=self.min_score,
                    must_not_regress=tuple(self.must_not_regress),
                ),
            ),
        )


def _job_json(job: FineTuneJob) -> dict[str, Any]:
    """A job as spec and status, the shape it was submitted in."""
    return {
        "name": job.name,
        "spec": {
            "tenant": job.spec.tenant,
            "base_model": job.spec.base_model,
            "trainer": job.spec.trainer,
            "trainer_version": job.spec.trainer_version,
            "dataset": {
                "uri": job.spec.dataset.uri,
                "checksum": job.spec.dataset.checksum,
                "rows": job.spec.dataset.rows,
                "schema_version": job.spec.dataset.schema_version,
            },
            "hyperparameters": job.spec.hyperparameters,
            "budget_ref": job.spec.budget_ref,
            "eval_suite": job.spec.eval_suite,
            "promotion_gate": {
                "min_score": job.spec.promotion_gate.min_score,
                "must_not_regress": list(job.spec.promotion_gate.must_not_regress),
            },
            "canary_steps": list(job.spec.canary_steps),
        },
        "status": {
            "phase": str(job.status.phase),
            "external_id": job.status.external_id,
            "artifact_ref": job.status.artifact_ref,
            "reason": job.status.reason,
            "cost_micro_usd": job.status.cost_micro_usd,
            "attempts": job.status.attempts,
            "scorecard": _scorecard_json(job.status.scorecard),
            "baseline": _scorecard_json(job.status.baseline),
            "rollout": {
                # -1 rather than null for "not started": a client walking the
                # steps needs to tell "no rollout" from "at step 0, weight 0",
                # and those are different situations.
                "step": job.status.rollout_step,
                "weight": job.status.rollout_weight,
                "adapter_id": job.adapter_id if job.rolling_out else "",
            },
        },
    }


def _scorecard_json(card: Scorecard | None) -> dict[str, Any] | None:
    """What a suite measured. Null when nothing has measured it yet."""
    if card is None:
        return None
    return {
        "score": card.score,
        "suite": card.suite,
        "suite_version": card.suite_version,
        "metrics": [
            {
                "name": m.name,
                "value": m.value,
                # Which way is better travels with the number, so a client
                # comparing two scorecards cannot get the direction wrong.
                "direction": str(m.direction),
                "unit": m.unit,
            }
            for m in card.metrics
        ],
    }


def _component_json(component: Component) -> dict[str, Any]:
    manifest = component.manifest
    admission = component.admission
    return {
        "name": manifest.name,
        "version": manifest.version,
        "port": str(manifest.port),
        "status": str(component.status),
        "digest": manifest.digest(),
        "latency_budget_ms": manifest.latency_budget_ms,
        "failure_mode": str(manifest.failure_mode),
        "execution": str(manifest.execution),
        "capabilities": list(manifest.capabilities),
        "image": manifest.image,
        "module": manifest.module,
        # Who vouched for this, as evidence rather than as a verdict: the
        # signature is here so it can be re-checked, and the snapshot builder
        # does exactly that before binding the component.
        "signing_key_id": component.signature.key_id if component.signature else "",
        "signature": component.signature.encoded() if component.signature else "",
        "admission": None
        if admission is None
        else {
            "suite": str(admission.suite),
            "suite_version": admission.suite_version,
            "manifest_digest": admission.manifest_digest,
            "passed": admission.passed,
            "runner": admission.runner,
            "evidence_ref": admission.evidence_ref,
        },
    }


def _trust_tier(name: str) -> TrustTier:
    try:
        return TrustTier[name.upper()]
    except KeyError:
        raise InvalidRequestError(f"unknown trust tier {name!r}") from None


def _json_response(code: int, body: Any) -> Response:
    return Response(content=json.dumps(body), media_type="application/json", status_code=code)


def _json_str(value: str) -> str:
    return json.dumps(value)
