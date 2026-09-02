"""Read the source of truth into the domain model the builder consumes.

This is the only module that knows both the row shape and the domain shape.
Everything upstream of it works in dataclasses; everything downstream works in
tables. Keeping the translation in one place is what lets the schema be
normalised for storage and the domain be shaped for reasoning, without either
distorting the other.

# On ancestry

A key's org, team, user and application are resolved by following ordinary
parent references, not by consulting a closure table. See
docs/adr/0005-no-closure-table.md — the traversal happens once per snapshot
build over a table sized by the number of API keys, and the data plane never
queries anything at request time.
"""

from __future__ import annotations

import logging
from dataclasses import replace
from datetime import datetime

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.db.timestamps import as_utc
from model_gateway_control.domain.budget import Budget, BudgetScope
from model_gateway_control.domain.catalog import (
    Capability,
    Cost,
    Deployment,
    ModelAlias,
    RoutingKey,
    TrustTier,
)
from model_gateway_control.domain.component import (
    Admission,
    Component,
    Execution,
    FailureMode,
    Manifest,
    Port,
    Registry,
    Status,
)
from model_gateway_control.domain.identity import BudgetRef, Principal, RateLimit
from model_gateway_control.domain.policy import PolicyBundle, PolicyEffect, PolicyRule
from model_gateway_control.domain.signing import Signature
from model_gateway_control.domain.tenant import Fleet, PluginBinding, Tenant
from model_gateway_control.errors import NotFoundError

logger = logging.getLogger(__name__)


class Repository:
    """Loads configuration for the snapshot builder.

    Takes a session rather than creating one, so a caller controls the
    transaction boundary and a test can hand it a rolled-back session.
    """

    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    async def load_fleet(self) -> Fleet:
        """Read the tenant-independent half of a snapshot."""
        state = await self._session.get(models.FleetState, 1)
        if state is None:
            raise NotFoundError(
                "fleet state has not been initialised; run a migration and seed row 1"
            )

        deployments = (await self._session.scalars(select(models.Deployment))).all()
        aliases = (
            await self._session.scalars(
                select(models.Alias).where(models.Alias.tenant_id.is_(None))
            )
        ).all()
        plugins = (
            await self._session.scalars(
                select(models.PluginBinding).where(models.PluginBinding.tenant_id.is_(None))
            )
        ).all()

        return Fleet(
            default_policy=await self._load_policy(None),
            version=state.version,
            deployments=(
                tuple(_to_deployment(d) for d in deployments)
                + await self._adapter_deployments([_to_deployment(d) for d in deployments])
            ),
            aliases=tuple(_to_alias(a) for a in aliases),
            registry=await self.load_registry(),
            default_plugins=tuple(_to_plugin(p) for p in plugins),
            policy_bundle_ref=state.policy_bundle_ref,
        )

    async def _adapter_deployments(self, base: list[Deployment]) -> tuple[Deployment, ...]:
        """Deployments for the adapters that are rolling out.

        Derived rather than stored. An adapter is not a new endpoint — it is the
        base model's deployment serving a second routing key, which is what
        multi-LoRA means: one vLLM pod holds many adapters and loads them by id.
        Copying the base deployment and changing only the key and the weight
        keeps that true by construction. A stored copy would drift the moment
        someone repointed the base model at a new provider.

        Only jobs that have started a rollout appear. One at weight zero is in
        the routing table and not serving, which is where a rollout starts and
        where an abort returns it — and being in the table is what makes
        rollback a snapshot version rather than a redeployment.
        """
        rolling = (
            await self._session.scalars(
                select(models.FineTuneJob).where(models.FineTuneJob.rollout_step >= 0)
            )
        ).all()
        if not rolling:
            return ()

        by_model = {d.key.base_model: d for d in base if not d.key.adapter_id}

        adapters = []
        for row in rolling:
            host = by_model.get(row.base_model)
            if host is None:
                # The base model has no deployment, so there is nothing to load
                # the adapter into. Skipping rather than failing the whole
                # snapshot: one job pointing at a retired model must not stop
                # every other tenant's configuration from compiling.
                logger.warning(
                    "adapter %s/%s names base model %r, which has no deployment",
                    row.tenant_id,
                    row.name,
                    row.base_model,
                )
                continue
            adapters.append(
                replace(
                    host,
                    id=f"{host.id}+{row.tenant_id}-{row.name}",
                    key=RoutingKey(base_model=row.base_model, adapter_id=row.name),
                    weight=row.rollout_weight,
                    # Mirrored only while it serves nobody. Once real traffic
                    # reaches it there is nothing left for a shadow to say.
                    shadow_percent=row.shadow_percent if row.rollout_weight == 0 else 0,
                )
            )
        return tuple(adapters)

    async def load_registry(self) -> Registry:
        """Read the component registry the snapshot's bindings are checked against.

        Retired components are loaded too. A snapshot cannot bind one, but the
        error an operator gets should be "presidio@2.1.0 is retired" rather
        than "no such component", which sends them looking for a typo.
        """
        rows = (await self._session.scalars(select(models.Component))).all()
        return Registry(tuple(to_component(row) for row in rows))

    async def load_tenants(self) -> list[Tenant]:
        """Read every tenant's layer."""
        rows = (await self._session.scalars(select(models.Tenant))).all()
        return [await self.load_tenant(row.id) for row in rows]

    async def load_tenant(self, tenant_id: str) -> Tenant:
        """Read one tenant's layer."""
        row = await self._session.get(models.Tenant, tenant_id)
        if row is None:
            raise NotFoundError(f"no tenant {tenant_id!r}")

        budgets = tuple(_to_budget(b) for b in row.budgets)
        # Scopes are read once here rather than looked up per budget reference.
        # A query inside the principal loop would be one round trip per budget
        # per key — invisible with three keys, and the reason a snapshot build
        # takes minutes with thirty thousand.
        scopes = {b.id: b.scope for b in budgets}

        keys = (
            await self._session.scalars(
                select(models.ApiKey).where(
                    models.ApiKey.tenant_id == tenant_id,
                    # A revoked key is kept for the audit trail and excluded
                    # from the snapshot, rather than deleted.
                    models.ApiKey.revoked_at.is_(None),
                )
            )
        ).all()

        principals: list[Principal] = []
        lookups: dict[bytes, str] = {}
        for key in keys:
            principals.append(await self._to_principal(key, tenant_id, scopes))
            lookups[bytes(key.lookup)] = key.id

        aliases = (
            await self._session.scalars(
                select(models.Alias).where(models.Alias.tenant_id == tenant_id)
            )
        ).all()
        plugins = (
            await self._session.scalars(
                select(models.PluginBinding).where(models.PluginBinding.tenant_id == tenant_id)
            )
        ).all()

        return Tenant(
            id=row.id,
            tier=row.tier,
            version=row.version,
            key_prefixes=tuple(sorted(p.prefix for p in row.prefixes)),
            principals=tuple(principals),
            keys=lookups,
            alias_overrides=tuple(_to_alias(a) for a in aliases),
            budgets=budgets,
            plugins=tuple(_to_plugin(p) for p in plugins),
            policy=await self._load_policy(tenant_id),
            min_trust_tier=TrustTier(row.min_trust_tier),
        )

    async def _to_principal(
        self, key: models.ApiKey, tenant_id: str, scopes: dict[str, BudgetScope]
    ) -> Principal:
        """Flatten a key and its ancestry into the record the data plane reads.

        This is where the identity graph stops being a graph. Every ancestor is
        resolved once, here, so that admission is a hash lookup rather than a
        traversal — which is the part of the plan's §5.1 that actually matters.
        """
        team: models.Team | None = None
        user_id = ""
        app_id = ""

        if key.application is not None:
            app_id = key.application.id
            team = await self._session.get(models.Team, key.application.team_id)
        elif key.user is not None:
            user_id = key.user.id
            team = await self._session.get(models.Team, key.user.team_id)

        org_id = ""
        team_id = ""
        if team is not None:
            team_id = team.id
            org = await self._session.get(models.Org, team.org_id)
            if org is not None:
                org_id = org.id

        return Principal(
            key_id=key.id,
            tenant=tenant_id,
            org=org_id,
            team=team_id,
            user=user_id,
            app=app_id,
            roles=tuple(sorted(r.role for r in key.roles)),
            models_allow_all=key.models_allow_all,
            models=tuple(sorted(m.model for m in key.models)),
            budgets=tuple(
                BudgetRef(id=b.budget_id, scope=scopes[b.budget_id])
                for b in sorted(key.budgets, key=lambda b: b.budget_id)
                # A budget attached to a key but absent from the tenant is a
                # dangling reference. It is dropped here so the builder's own
                # check reports it as what it is, rather than this raising a
                # KeyError that names nothing useful.
                if b.budget_id in scopes
            ),
            default_data_class=key.default_data_class,
            min_trust_tier=TrustTier(key.min_trust_tier),
            limits=RateLimit(
                requests_per_minute=key.requests_per_minute,
                tokens_per_minute=key.tokens_per_minute,
                max_concurrent=key.max_concurrent,
            ),
            deprecated=key.deprecated,
            not_after=_as_utc(key.not_after),
        )

    async def _load_policy(self, tenant_id: str | None) -> PolicyBundle | None:
        """Read a policy in evaluation order, or None when there is none.

        None rather than an empty bundle, so "this tenant has no policy of its
        own" is distinguishable from "this tenant has an empty one" — the first
        falls back to the fleet default and the second deliberately does not.
        """
        where = (
            models.PolicyRule.tenant_id.is_(None)
            if tenant_id is None
            else models.PolicyRule.tenant_id == tenant_id
        )
        rows = (
            await self._session.scalars(
                select(models.PolicyRule).where(where).order_by(models.PolicyRule.position)
            )
        ).all()
        if not rows:
            return None

        return PolicyBundle(
            id=tenant_id or "fleet",
            rules=tuple(to_policy_rule(row) for row in rows),
        )


#: Which rule field each condition kind carries, in one place.
#:
#: Both directions read this. They were written separately once, with the
#: writer storing "models" and the reader looking for "model", so every model
#: condition was stored and then silently dropped — a policy that named a model
#: matched every model instead. One mapping is the fix; two mappings of one
#: thing is the bug.
POLICY_CONDITION_KINDS = {
    "model": "models",
    "endpoint": "endpoints",
    "role": "roles",
    "region": "regions",
    "cidr": "source_cidrs",
}


def to_policy_rule(row: models.PolicyRule) -> PolicyRule:
    """Map a stored rule to its domain form.

    Public because writing rules needs the same mapping read back, and a second
    copy of it is exactly how the two came to disagree.
    """
    by_kind: dict[str, list[str]] = {}
    for condition in row.conditions:
        by_kind.setdefault(condition.kind, []).append(condition.value)

    return PolicyRule(
        id=row.rule_id,
        effect=PolicyEffect(row.effect),
        models=tuple(sorted(by_kind.get("model", []))),
        endpoints=tuple(sorted(by_kind.get("endpoint", []))),
        roles=tuple(sorted(by_kind.get("role", []))),
        regions=tuple(sorted(by_kind.get("region", []))),
        source_cidrs=tuple(sorted(by_kind.get("cidr", []))),
        max_payload_bytes=row.max_payload_bytes,
        data_class=row.data_class,
        min_trust_tier=TrustTier(row.min_trust_tier),
        reason=row.reason,
    )


def _as_utc(when: datetime | None) -> datetime | None:
    """:func:`as_utc`, for a column that is nullable."""
    return None if when is None else as_utc(when)


def _to_deployment(row: models.Deployment) -> Deployment:
    return Deployment(
        id=row.id,
        key=RoutingKey(base_model=row.base_model, adapter_id=row.adapter_id),
        provider=row.provider,
        endpoint=row.endpoint,
        region=row.region,
        trust_tier=TrustTier(row.trust_tier),
        credential_ref=row.credential_ref,
        weight=row.weight,
        cost=Cost(
            input_per_1k_micro_usd=row.input_cost_micro_usd,
            output_per_1k_micro_usd=row.output_cost_micro_usd,
            cached_input_per_1k_micro_usd=row.cached_input_cost_micro_usd,
            cache_write_per_1k_micro_usd=row.cache_write_cost_micro_usd,
        ),
        capabilities=tuple(
            Capability(c.capability) for c in sorted(row.capabilities, key=lambda c: c.capability)
        ),
    )


def _to_alias(row: models.Alias) -> ModelAlias:
    return ModelAlias(
        name=row.name,
        targets=tuple(
            RoutingKey(base_model=t.base_model, adapter_id=t.adapter_id) for t in row.targets
        ),
    )


def to_manifest(row: models.Component) -> Manifest:
    """Map a component row to the manifest it stores.

    Separate from ``to_component`` because the manifest alone is well defined
    even when the row is not: an active row whose admission no longer covers
    its manifest is a corrupt registry, and the registry service needs to read
    the manifest in order to say so.
    """
    return Manifest(
        name=row.name,
        version=row.version,
        port=Port(row.port),
        config_schema=row.config_schema,
        latency_budget_ms=row.latency_budget_ms,
        failure_mode=FailureMode(row.failure_mode),
        execution=Execution(row.execution),
        capabilities=tuple(sorted(c.name for c in row.capabilities)),
        image=row.image,
        module=row.module,
    )


def to_signature(row: models.Component) -> Signature | None:
    """Map a component row's signature columns, if it carries one.

    A row with a key id but no signature — or the reverse — is treated as
    unsigned rather than as an error: only half a signature proves nothing, and
    the policy check that follows decides whether proving nothing is allowed.
    """
    if not row.signing_key_id or not row.signature:
        return None
    return Signature.decode(row.signing_key_id, row.signature)


def to_component(row: models.Component) -> Component:
    """Map a component row to its domain form, invariants and all.

    Public — unlike its siblings here — because the registry service needs the
    same mapping, and a second copy of it is how a status or a digest ends up
    interpreted two different ways.
    """
    # The latest run is the current admission; earlier ones are history. Rows
    # are ordered by id in the relationship, so this does not depend on the
    # database returning them in insertion order.
    latest = row.admissions[-1] if row.admissions else None
    return Component(
        manifest=to_manifest(row),
        status=Status(row.status),
        admission=_to_admission(latest) if latest is not None else None,
        signature=to_signature(row),
    )


def _to_admission(row: models.ComponentAdmission) -> Admission:
    return Admission(
        suite=Port(row.suite),
        suite_version=row.suite_version,
        manifest_digest=row.manifest_digest,
        passed=row.passed,
        runner=row.runner,
        evidence_ref=row.evidence_ref,
        recorded_at=row.recorded_at,
    )


def _to_plugin(row: models.PluginBinding) -> PluginBinding:
    return PluginBinding(
        port=Port(row.port),
        component=row.component,
        version=row.version,
        config_ref=row.config_ref,
    )


def _to_budget(row: models.Budget) -> Budget:
    return Budget(
        id=row.id,
        scope=BudgetScope(row.scope),
        limit_micro_usd=row.limit_micro_usd,
        spent_micro_usd=row.spent_micro_usd,
        hard=row.hard,
        headroom_basis_points=row.headroom_basis_points,
    )
