"""What the builder needs to know about one tenant, and about the fleet."""

from __future__ import annotations

from dataclasses import dataclass, field

from model_gateway_control.domain.budget import Budget
from model_gateway_control.domain.catalog import Deployment, ModelAlias, TrustTier
from model_gateway_control.domain.identity import Principal
from model_gateway_control.errors import InvalidRequestError


@dataclass(frozen=True, slots=True)
class PluginBinding:
    """Which registry component fills a port, at which version."""

    port: str
    component: str
    version: str = ""
    config_ref: str = ""


@dataclass(frozen=True, slots=True, kw_only=True)
class Tenant:
    """One tenant's configuration: small, and changing constantly."""

    id: str
    #: Plan tier. Safe as a metrics label, unlike the tenant id, which is
    #: unbounded cardinality and will take Prometheus down.
    tier: str = "unknown"
    version: int = 1
    #: Key prefixes routed to this tenant. More than one exists during a prefix
    #: migration.
    key_prefixes: tuple[str, ...] = ()
    principals: tuple[Principal, ...] = ()
    #: Lookup bytes to key id. The control plane is the only component that ever
    #: sees a key in clear text, and only at issuance.
    keys: dict[bytes, str] = field(default_factory=dict)
    alias_overrides: tuple[ModelAlias, ...] = ()
    budgets: tuple[Budget, ...] = ()
    plugins: tuple[PluginBinding, ...] = ()
    #: Floor on destination trust for every request from this tenant. Data
    #: residency is expressed here, not as a deployment afterthought.
    min_trust_tier: TrustTier = TrustTier.EXTERNAL

    def __post_init__(self) -> None:
        if not self.id:
            raise InvalidRequestError("a tenant needs an id")
        if self.version < 1:
            raise InvalidRequestError(f"tenant {self.id!r} version must be at least 1")
        if not self.key_prefixes:
            # A tenant no prefix routes to would build cleanly and then
            # authenticate nobody, which is far harder to diagnose than a
            # failed build.
            raise InvalidRequestError(f"tenant {self.id!r} has no key prefix")


@dataclass(frozen=True, slots=True, kw_only=True)
class Fleet:
    """The tenant-independent half: large, and changing rarely."""

    version: int = 1
    deployments: tuple[Deployment, ...] = ()
    aliases: tuple[ModelAlias, ...] = ()
    default_plugins: tuple[PluginBinding, ...] = ()
    policy_bundle_ref: str = ""

    def __post_init__(self) -> None:
        if self.version < 1:
            raise InvalidRequestError("fleet version must be at least 1")
