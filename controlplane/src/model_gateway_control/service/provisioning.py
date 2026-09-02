"""Creating the things a gateway serves: tenants, models, keys, budgets.

Everything else in this control plane reads. This is the half that writes, and
until it existed onboarding a model meant hand-writing an INSERT against a
table with thirteen NOT NULL columns and no defaults — which is three attempts
and a constraint error before anything works, every time, for everyone.

# Declarative, because a program is the caller

Each of these takes the desired state and makes it so, whether or not it
already existed. A compliance engine or a deploy script restating its position
should be able to send that position and be done, without first working out
what it said last time — and a retry after a crash must not be a second
tenant.

That is why these are ``ensure`` rather than ``create``: the name says the
caller is describing a destination, not requesting an action.

# What is deliberately not idempotent

Issuing a key. A key is a secret that exists in clear text exactly once, so
"make sure a key exists" cannot be re-run — the second call has nothing to
return. Keys are created, listed and revoked, and rotation is its own operation
because replacing a secret while the old one still works is a different thing
from making a new one.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.domain.budget import BudgetScope
from model_gateway_control.domain.catalog import Capability, TrustTier
from model_gateway_control.errors import InvalidRequestError, NotFoundError


@dataclass(frozen=True, slots=True, kw_only=True)
class DeploymentSpec:
    """A place a model can be served from.

    Defaults for everything the gateway can reasonably assume, because the
    table cannot: it has thirteen NOT NULL columns with no defaults, and making
    a caller supply all of them is how a deployment ends up with a cost of zero
    that nobody notices until the invoice.
    """

    id: str
    base_model: str
    provider: str
    endpoint: str
    trust_tier: TrustTier
    #: Empty for a plain model. A fine-tuned adapter is the same base model
    #: with an adapter id, served by the same deployment.
    adapter_id: str = ""
    region: str = ""
    credential_ref: str = ""
    #: Serving by default. Zero means registered and taking no traffic, which
    #: is where a canary starts — not where a model an operator just added
    #: should sit, silently doing nothing.
    weight: int = 100
    input_cost_micro_usd: int = 0
    output_cost_micro_usd: int = 0
    cached_input_cost_micro_usd: int = 0
    cache_write_cost_micro_usd: int = 0
    capabilities: tuple[Capability, ...] = ()

    def __post_init__(self) -> None:
        if not self.id or not self.base_model:
            raise InvalidRequestError("a deployment needs an id and a base model")
        if not self.endpoint:
            raise InvalidRequestError(f"deployment {self.id!r} needs an endpoint")
        if not 0 <= self.weight <= 100:
            raise InvalidRequestError(f"deployment {self.id!r} has weight {self.weight}")
        if self.trust_tier is TrustTier.UNSET:
            # An unset tier would be filtered out of every candidate list, so
            # the deployment would exist and never serve.
            raise InvalidRequestError(f"deployment {self.id!r} needs a trust tier")


class ProvisioningService:
    """Writes the configuration a snapshot is compiled from."""

    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    # --- tenants and the identity hierarchy -------------------------------

    async def ensure_tenant(
        self,
        tenant_id: str,
        *,
        tier: str = "standard",
        min_trust_tier: TrustTier = TrustTier.EXTERNAL,
    ) -> models.Tenant:
        """A tenant, its key prefix, and somewhere to hang keys.

        The hierarchy is created alongside rather than left to the caller. A
        tenant with no application cannot own a key, so making that a separate
        call only means everyone makes both calls — and whoever forgets gets a
        foreign-key error rather than an explanation.
        """
        if not tenant_id:
            raise InvalidRequestError("a tenant needs an id")

        row = await self._session.get(models.Tenant, tenant_id)
        if row is None:
            row = models.Tenant(
                id=tenant_id, tier=tier, version=1, min_trust_tier=int(min_trust_tier)
            )
            self._session.add(row)
        else:
            row.tier = tier
            row.min_trust_tier = int(min_trust_tier)
            self._bump(row)

        # Each child checked separately, not just when the tenant is new.
        # "Ensure" has to mean ensure: a tenant created some other way — by an
        # older migration, by a seed script, by hand — would otherwise be left
        # without an application, and the failure surfaces later as a foreign
        # key error when someone tries to issue it a key.
        await self._ensure(models.KeyPrefix, tenant_id, prefix=tenant_id, tenant_id=tenant_id)
        await self._ensure(
            models.Org,
            f"{tenant_id}-org",
            id=f"{tenant_id}-org",
            tenant_id=tenant_id,
            name=tenant_id,
        )
        await self._ensure(
            models.Team,
            f"{tenant_id}-team",
            id=f"{tenant_id}-team",
            org_id=f"{tenant_id}-org",
            name=tenant_id,
        )
        await self._ensure(
            models.Application,
            f"{tenant_id}-app",
            id=f"{tenant_id}-app",
            team_id=f"{tenant_id}-team",
            name=tenant_id,
        )

        await self._session.flush()
        return row

    async def _ensure(self, model: type[Any], key: str, **fields: Any) -> None:
        """Create a row if it is not already there."""
        existing = await self._session.get(model, key)
        if existing is None:
            self._session.add(model(**fields))
            await self._session.flush()

    # --- deployments ------------------------------------------------------

    async def ensure_deployment(self, spec: DeploymentSpec) -> models.Deployment:
        """Make a model servable, or update where it is served from."""
        row = await self._session.get(models.Deployment, spec.id)
        if row is None:
            row = models.Deployment(id=spec.id)
            self._session.add(row)

        row.base_model = spec.base_model
        row.adapter_id = spec.adapter_id
        row.provider = spec.provider
        row.endpoint = spec.endpoint
        row.region = spec.region
        row.trust_tier = int(spec.trust_tier)
        row.credential_ref = spec.credential_ref
        row.weight = spec.weight
        row.input_cost_micro_usd = spec.input_cost_micro_usd
        row.output_cost_micro_usd = spec.output_cost_micro_usd
        row.cached_input_cost_micro_usd = spec.cached_input_cost_micro_usd
        row.cache_write_cost_micro_usd = spec.cache_write_cost_micro_usd
        # Replaced rather than merged: capabilities are a description of what
        # this endpoint can do, and a stale one left behind would route
        # streaming traffic somewhere that cannot stream.
        row.capabilities = [models.DeploymentCapability(name=str(c)) for c in spec.capabilities]

        await self._session.flush()
        return row

    async def remove_deployment(self, deployment_id: str) -> None:
        """Take a model out of the catalog.

        Deleting rather than retiring, unlike a component: a deployment is a
        place, and a place that no longer exists should stop being offered. The
        audit of what it served lives in usage records, which outlive it.
        """
        row = await self._session.get(models.Deployment, deployment_id)
        if row is None:
            raise NotFoundError(f"no deployment {deployment_id!r}")
        await self._session.delete(row)
        await self._session.flush()

    async def list_deployments(self) -> list[models.Deployment]:
        """Every deployment, by id."""
        return list(
            (
                await self._session.scalars(
                    select(models.Deployment).order_by(models.Deployment.id)
                )
            ).all()
        )

    # --- aliases ----------------------------------------------------------

    async def ensure_alias(
        self, name: str, targets: list[str], tenant_id: str | None = None
    ) -> None:
        """Point a friendly name at one or more base models, in order."""
        if not targets:
            raise InvalidRequestError(f"alias {name!r} needs at least one target")

        existing = await self._session.scalar(
            select(models.Alias).where(
                models.Alias.name == name,
                models.Alias.tenant_id.is_(None)
                if tenant_id is None
                else models.Alias.tenant_id == tenant_id,
            )
        )
        if existing is not None:
            await self._session.delete(existing)
            await self._session.flush()

        alias = models.Alias(tenant_id=tenant_id, name=name)
        alias.targets = [
            models.AliasTarget(position=i, base_model=target) for i, target in enumerate(targets)
        ]
        self._session.add(alias)
        await self._session.flush()

    # --- budgets ----------------------------------------------------------

    async def ensure_budget(
        self,
        budget_id: str,
        *,
        tenant_id: str,
        limit_micro_usd: int,
        scope: BudgetScope = BudgetScope.ORG,
        hard: bool = True,
        headroom_basis_points: int = 500,
    ) -> models.Budget:
        """A spending limit.

        Spend is never reset here. A limit can be raised, lowered or made soft,
        but "what has been spent" is a fact the accounting consumer owns —
        letting provisioning zero it would make the arithmetic disagree with
        the invoice, silently and in the flattering direction.
        """
        if limit_micro_usd < 0:
            raise InvalidRequestError(f"budget {budget_id!r} has a negative limit")

        row = await self._session.get(models.Budget, budget_id)
        if row is None:
            row = models.Budget(
                id=budget_id, tenant_id=tenant_id, spent_micro_usd=0, scope=int(scope)
            )
            self._session.add(row)
        row.limit_micro_usd = limit_micro_usd
        row.scope = int(scope)
        row.hard = hard
        row.headroom_basis_points = headroom_basis_points

        await self._bump_tenant(tenant_id)
        await self._session.flush()
        return row

    # --- helpers ----------------------------------------------------------

    async def _bump_tenant(self, tenant_id: str) -> None:
        row = await self._session.get(models.Tenant, tenant_id)
        if row is None:
            raise NotFoundError(f"no tenant {tenant_id!r}")
        self._bump(row)

    @staticmethod
    def _bump(row: models.Tenant) -> None:
        """Advance a tenant's layer version.

        Workers refuse a layer whose version moved backwards, so anything that
        changes a tenant's configuration has to move it forwards.
        """
        row.version += 1
