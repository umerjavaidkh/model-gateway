"""Writing a policy: the seam an external authority publishes rules through.

The gateway evaluates policy; it does not decide what the policy should be.
Something else — a compliance engine, an operator, an agent — decides, and this
is where that decision lands.

# Declarative, because the writer is a program

A rule set is replaced whole rather than patched. A compliance engine restating
its current position must be able to send that position and be done, without
first working out which rules it added last time. Anything else makes the
writer keep a model of what the gateway believes, and two models of one thing
disagree eventually — usually about a rule someone thought they had removed.

Replacing whole also makes retries free, which matters when the writer is a
program that may crash mid-publish.

# Ordered, because first match wins

Position is the whole of the conflict resolution: the order the author wrote is
the order that runs. That is why this stores a list rather than a set.
"""

from __future__ import annotations

from collections.abc import Sequence

from sqlalchemy import delete, select
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.db.repository import POLICY_CONDITION_KINDS, to_policy_rule
from model_gateway_control.domain.policy import PolicyBundle, PolicyRule
from model_gateway_control.errors import NotFoundError


class PolicyService:
    """Reads and replaces the rules a snapshot compiles."""

    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    async def replace(self, tenant: str | None, rules: Sequence[PolicyRule]) -> PolicyBundle:
        """Make these rules, in this order, the policy for a scope.

        ``tenant`` is None for the fleet default. A tenant's own rules replace
        the default rather than merging with it, which is the same rule the
        snapshot applies — two ordered lists merged have no defensible order.
        """
        if tenant is not None:
            exists = await self._session.scalar(
                select(models.Tenant).where(models.Tenant.id == tenant)
            )
            if exists is None:
                raise NotFoundError(f"no tenant {tenant!r}")

        # Constructed before anything is deleted, so a rule set that will not
        # validate leaves the previous one in place. A publish that half-applies
        # is worse than one that fails.
        bundle = PolicyBundle(id=tenant or "fleet", rules=tuple(rules))

        where = (
            models.PolicyRule.tenant_id.is_(None)
            if tenant is None
            else models.PolicyRule.tenant_id == tenant
        )
        await self._session.execute(delete(models.PolicyRule).where(where))

        for position, rule in enumerate(bundle.rules):
            row = models.PolicyRule(
                tenant_id=tenant,
                rule_id=rule.id,
                position=position,
                effect=str(rule.effect),
                max_payload_bytes=rule.max_payload_bytes,
                data_class=rule.data_class,
                min_trust_tier=int(rule.min_trust_tier),
                reason=rule.reason,
            )
            # The same mapping the reader uses. Kept in one place because
            # having two is how a stored condition came to be silently
            # unreadable.
            row.conditions = [
                models.PolicyCondition(kind=kind, value=value)
                for kind, field in POLICY_CONDITION_KINDS.items()
                for value in getattr(rule, field)
            ]
            self._session.add(row)

        await self._session.flush()
        return bundle

    async def get(self, tenant: str | None) -> PolicyBundle:
        """The rules currently in force for a scope.

        An empty bundle rather than None when there are none: a caller asking
        what the policy is has been answered, and "there are no rules" is an
        answer.
        """
        where = (
            models.PolicyRule.tenant_id.is_(None)
            if tenant is None
            else models.PolicyRule.tenant_id == tenant
        )
        rows = (
            await self._session.scalars(
                select(models.PolicyRule).where(where).order_by(models.PolicyRule.position)
            )
        ).all()
        return PolicyBundle(id=tenant or "fleet", rules=tuple(to_policy_rule(r) for r in rows))
