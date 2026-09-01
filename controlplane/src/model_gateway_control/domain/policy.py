"""Policy: an ordered decision table over a fixed attribute set.

This is deliberately not a language. It has no negation, no recursion, no joins
and no data documents — an ordered list of conjunctions, evaluated first-match
wins. See docs/adr/0006-compiled-policy-not-a-policy-language.md for why, and
for what would change it.

The shape here is the *authoring* format, and it is expected to be replaced.
When policy is authored elsewhere — in a dedicated policy service, or in Cedar —
that becomes a front end which emits the same compiled bundle, and nothing in
the data plane changes because the data plane never sees this.
"""

from __future__ import annotations

import ipaddress
from dataclasses import dataclass
from enum import StrEnum

from model_gateway_control.domain.catalog import TrustTier
from model_gateway_control.errors import InvalidRequestError


class PolicyEffect(StrEnum):
    """What a matching rule does."""

    ALLOW = "allow"
    DENY = "deny"


@dataclass(frozen=True, slots=True, kw_only=True)
class PolicyRule:
    """One row of the decision table.

    Every condition is a set the attribute must be in, and an empty set matches
    anything. All present conditions must hold, so a rule reads as a conjunction
    and can be understood without reference to any other rule.
    """

    id: str
    effect: PolicyEffect

    models: tuple[str, ...] = ()
    endpoints: tuple[str, ...] = ()
    roles: tuple[str, ...] = ()
    regions: tuple[str, ...] = ()
    source_cidrs: tuple[str, ...] = ()
    #: Upper bound on request size. Zero means no bound.
    max_payload_bytes: int = 0

    #: Applied when an allow rule matches. Stamping sensitivity here is what
    #: lets the router turn it into a destination constraint.
    data_class: str = ""
    min_trust_tier: TrustTier = TrustTier.UNSET

    #: Returned to the caller on a denial, so it is expected to be safe to
    #: disclose. Operators write it; nothing derives it from the payload.
    reason: str = ""

    def __post_init__(self) -> None:
        if not self.id:
            raise InvalidRequestError("a policy rule needs an id")
        if self.max_payload_bytes < 0:
            raise InvalidRequestError(f"rule {self.id!r} has a negative payload bound")

        for cidr in self.source_cidrs:
            try:
                ipaddress.ip_network(cidr, strict=False)
            except ValueError as err:
                # Rejected here rather than at the worker. A network rule that
                # silently does not apply is a restriction an operator believes
                # is in force and is not — and finding that out at build time
                # costs a failed build, not an unnoticed hole.
                raise InvalidRequestError(
                    f"rule {self.id!r} has an unparseable network {cidr!r}"
                ) from err

        for endpoint in self.endpoints:
            if endpoint not in ("chat_completions", "messages", "embeddings"):
                raise InvalidRequestError(f"rule {self.id!r} names unknown endpoint {endpoint!r}")


@dataclass(frozen=True, slots=True, kw_only=True)
class PolicyBundle:
    """A compiled decision table, in evaluation order."""

    id: str = ""
    version: int = 1
    rules: tuple[PolicyRule, ...] = ()
    #: Applied when no rule matches. Allow, because policy is one control among
    #: several and a bundle that denied by default would make adding the
    #: feature an outage.
    default_effect: PolicyEffect = PolicyEffect.ALLOW

    def __post_init__(self) -> None:
        seen: set[str] = set()
        for rule in self.rules:
            if rule.id in seen:
                # Two rules with one id makes "which rule refused this" — the
                # question a denial exists to answer — unanswerable.
                raise InvalidRequestError(f"duplicate policy rule id {rule.id!r}")
            seen.add(rule.id)

    @property
    def empty(self) -> bool:
        return not self.rules
