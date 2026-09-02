"""What the builder needs to know about one tenant, and about the fleet."""

from __future__ import annotations

from dataclasses import dataclass, field

from model_gateway_control.domain.budget import Budget
from model_gateway_control.domain.catalog import Deployment, ModelAlias, TrustTier

# FailureMode and Port belong to the extension surface, so they are defined
# with the registry that governs it rather than duplicated here. A binding and
# the manifest it names have to agree about both, and two definitions of the
# same enum is how they stop agreeing.
from model_gateway_control.domain.component import FailureMode, Port, Registry
from model_gateway_control.domain.identity import Principal
from model_gateway_control.domain.policy import PolicyBundle
from model_gateway_control.errors import InvalidRequestError

__all__ = [
    "FailureMode",
    "Fleet",
    "GuardrailBinding",
    "PluginBinding",
    "Port",
    "Tenant",
]


@dataclass(frozen=True, slots=True)
class PluginBinding:
    """Which registry component fills a port, at which version.

    An empty version means "the one bindable version", which the registry
    resolves at snapshot build time and refuses when it is ambiguous.
    """

    port: Port
    component: str
    version: str = ""
    config_ref: str = ""


@dataclass(frozen=True, slots=True, kw_only=True)
class GuardrailBinding:
    """One guardrail and the budget it is admitted under.

    A list per tenant rather than one binding per port, because a tenant
    genuinely runs several: a secret scan that blocks, an injection heuristic
    that only alerts, a content policy. Modelling it as one-per-port would force
    them into one component and make their budgets indistinguishable.
    """

    component: str
    version: str = ""
    config_ref: str = ""
    #: Enforced by the data plane, not trusted to the guardrail. One that hangs
    #: must not be able to hang the request.
    timeout_ms: int = 50
    failure_mode: FailureMode = FailureMode.CLOSED
    #: Blocking guardrails run inline and may refuse or rewrite. Non-blocking
    #: ones run off the request path and can only alert.
    blocking: bool = True
    #: Legs to inspect. Empty means the request leg only.
    phases: tuple[str, ...] = ()

    def __post_init__(self) -> None:
        if not self.component:
            raise InvalidRequestError("a guardrail binding needs a component")
        if self.timeout_ms <= 0:
            raise InvalidRequestError(f"guardrail {self.component!r} needs a positive timeout")
        for phase in self.phases:
            if phase not in ("request", "response"):
                raise InvalidRequestError(f"unknown guardrail phase {phase!r}")


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
    #: Guardrails for this tenant. Declaring any replaces the fleet defaults
    #: entirely rather than merging: merging two lists of things that can refuse
    #: traffic produces a set nobody can predict.
    guardrails: tuple[GuardrailBinding, ...] = ()
    #: This tenant's policy. Replaces the fleet default rather than merging,
    #: for the same reason guardrails do: two ordered rule lists merged produce
    #: an order nobody can predict.
    policy: PolicyBundle | None = None
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
    #: The admitted component set every binding in this snapshot is checked
    #: against. Carried on the fleet rather than looked up during the build so
    #: that a snapshot is a pure function of its inputs: two builds from the
    #: same fleet produce the same bytes, whatever the registry does meanwhile.
    registry: Registry = field(default_factory=Registry)
    default_plugins: tuple[PluginBinding, ...] = ()
    #: Guardrails applied to any tenant declaring none of its own.
    default_guardrails: tuple[GuardrailBinding, ...] = ()
    #: Applied to any tenant with no policy of its own.
    default_policy: PolicyBundle | None = None
    policy_bundle_ref: str = ""

    def __post_init__(self) -> None:
        if self.version < 1:
            raise InvalidRequestError("fleet version must be at least 1")
