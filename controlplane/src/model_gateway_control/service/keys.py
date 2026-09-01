"""Issuing, rotating and revoking API keys.

# Two-generation rotation

The reference plan's §5.1 asks for it, and the reason is operational: a rotation
that invalidates the old key immediately is an outage for every caller that has
not redeployed yet. So rotating issues a *new* key and leaves the old one
working, marked deprecated, until its overlap window expires. The data plane
attaches a warning header when a deprecated key is used, which is how a tenant
finds the callers they forgot.

The old key is not deleted when the window closes. It is excluded from the next
snapshot, and the row stays for the audit trail.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.domain.catalog import TrustTier
from model_gateway_control.domain.identity import RateLimit, issue_key
from model_gateway_control.errors import ConflictError, InvalidRequestError, NotFoundError

#: How long a rotated key keeps working. Long enough for a deployment to roll
#: through every environment, short enough that a forgotten caller is found
#: rather than tolerated indefinitely.
DEFAULT_ROTATION_OVERLAP = timedelta(days=7)


@dataclass(frozen=True, slots=True)
class NewKey:
    """A key that has just been created.

    ``presented`` is the only time the secret exists outside the caller's hands.
    It is returned once and never stored; what persists is the HMAC lookup.
    """

    key_id: str
    presented: str

    def __repr__(self) -> str:
        return f"NewKey(key_id={self.key_id!r}, presented=<redacted>)"


class KeyService:
    """Key lifecycle against one session.

    The pepper is injected rather than read from configuration here, so a test
    can use its own and so the value has exactly one home in the process.
    """

    def __init__(
        self,
        session: AsyncSession,
        pepper: bytes,
        *,
        overlap: timedelta = DEFAULT_ROTATION_OVERLAP,
        now: Callable[[], datetime] | None = None,
    ) -> None:
        self._session = session
        self._pepper = pepper
        self._overlap = overlap
        self._now = now or (lambda: datetime.now(UTC))

    async def issue(
        self,
        *,
        tenant_id: str,
        key_id: str,
        application_id: str | None = None,
        user_id: str | None = None,
        models_allow_all: bool = False,
        min_trust_tier: TrustTier = TrustTier.EXTERNAL,
        limits: RateLimit | None = None,
    ) -> NewKey:
        """Mint a key for an application or a user."""
        if (application_id is None) == (user_id is None):
            # Exactly one owner. Neither means the key has no ancestry to
            # flatten; both means the principal's org and team are ambiguous.
            raise InvalidRequestError("a key belongs to exactly one application or user")

        tenant = await self._session.get(models.Tenant, tenant_id)
        if tenant is None:
            raise NotFoundError(f"no tenant {tenant_id!r}")
        if await self._session.get(models.ApiKey, key_id) is not None:
            raise ConflictError(f"key {key_id!r} already exists")

        prefix = await self._first_prefix(tenant_id)
        minted = issue_key(self._pepper, prefix, key_id)

        self._session.add(
            models.ApiKey(
                id=key_id,
                tenant_id=tenant_id,
                application_id=application_id,
                user_id=user_id,
                lookup=minted.lookup,
                models_allow_all=models_allow_all,
                min_trust_tier=int(min_trust_tier),
                requests_per_minute=(limits or RateLimit()).requests_per_minute,
                tokens_per_minute=(limits or RateLimit()).tokens_per_minute,
                max_concurrent=(limits or RateLimit()).max_concurrent,
            )
        )
        await self._bump_tenant_version(tenant)
        return NewKey(key_id=key_id, presented=minted.presented)

    async def rotate(self, key_id: str, *, new_key_id: str | None = None) -> NewKey:
        """Issue a successor and put the current key into its overlap window.

        Both keys work until the old one's ``not_after`` passes. That overlap is
        the whole point: without it, rotation is an outage for every caller that
        has not redeployed.
        """
        existing = await self._session.get(models.ApiKey, key_id)
        if existing is None:
            raise NotFoundError(f"no key {key_id!r}")
        if existing.revoked_at is not None:
            raise ConflictError(f"key {key_id!r} is revoked; issue a new one instead")

        successor_id = new_key_id or f"{key_id}-{int(self._now().timestamp())}"
        if await self._session.get(models.ApiKey, successor_id) is not None:
            raise ConflictError(f"key {successor_id!r} already exists")

        prefix = await self._first_prefix(existing.tenant_id)
        minted = issue_key(self._pepper, prefix, successor_id)

        successor = models.ApiKey(
            id=successor_id,
            tenant_id=existing.tenant_id,
            application_id=existing.application_id,
            user_id=existing.user_id,
            lookup=minted.lookup,
            models_allow_all=existing.models_allow_all,
            default_data_class=existing.default_data_class,
            min_trust_tier=existing.min_trust_tier,
            # Limits carry across, for the same reason roles and budgets do: a
            # rotated key that silently gets different limits looks like a
            # capacity problem in the caller.
            requests_per_minute=existing.requests_per_minute,
            tokens_per_minute=existing.tokens_per_minute,
            max_concurrent=existing.max_concurrent,
        )
        self._session.add(successor)
        await self._session.flush()

        # The successor inherits the predecessor's grants. A rotated key that
        # silently loses its roles or budgets would look like a permissions bug
        # in the caller.
        for role in existing.roles:
            self._session.add(models.KeyRole(key_id=successor_id, role=role.role))
        for allowed in existing.models:
            self._session.add(models.KeyModel(key_id=successor_id, model=allowed.model))
        for budget in existing.budgets:
            self._session.add(models.KeyBudget(key_id=successor_id, budget_id=budget.budget_id))

        existing.deprecated = True
        existing.not_after = self._now() + self._overlap

        tenant = await self._session.get(models.Tenant, existing.tenant_id)
        if tenant is not None:
            await self._bump_tenant_version(tenant)
        return NewKey(key_id=successor_id, presented=minted.presented)

    async def revoke(self, key_id: str) -> None:
        """Stop a key working at the next snapshot.

        Revoking marks rather than deletes. Deleting would take the audit trail
        with it, and the audit trail is the reason anyone can answer what that
        key did.
        """
        existing = await self._session.get(models.ApiKey, key_id)
        if existing is None:
            raise NotFoundError(f"no key {key_id!r}")
        if existing.revoked_at is not None:
            return  # Idempotent: revoking twice is not an error.

        existing.revoked_at = self._now()
        tenant = await self._session.get(models.Tenant, existing.tenant_id)
        if tenant is not None:
            await self._bump_tenant_version(tenant)

    async def _first_prefix(self, tenant_id: str) -> str:
        prefixes = (
            await self._session.scalars(
                select(models.KeyPrefix.prefix)
                .where(models.KeyPrefix.tenant_id == tenant_id)
                .order_by(models.KeyPrefix.prefix)
            )
        ).all()
        if not prefixes:
            raise InvalidRequestError(
                f"tenant {tenant_id!r} has no key prefix, so a key cannot be routed to it"
            )
        return prefixes[0]

    async def _bump_tenant_version(self, tenant: models.Tenant) -> None:
        """Advance the tenant's layer version.

        Every key change alters that tenant's snapshot layer, and a worker
        rejects a layer whose version has not moved forward — so forgetting this
        would make the change invisible until something else happened to bump it.
        """
        tenant.version += 1
        await self._session.flush()
