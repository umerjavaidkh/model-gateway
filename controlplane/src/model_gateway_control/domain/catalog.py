"""Model catalog: what can be called, and where it is served."""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import IntEnum, StrEnum

from model_gateway_control.errors import InvalidRequestError


class TrustTier(IntEnum):
    """Where a request is allowed to be sent.

    Ordered, and higher means more trusted, because admission computes a
    *minimum* acceptable tier and the router filters candidates against it.
    ``UNSET`` exists so a deployment cannot silently default to the most
    permissive tier.
    """

    UNSET = 0
    EXTERNAL = 1
    PRIVATE_CLOUD = 2
    INTERNAL = 3


class Capability(StrEnum):
    """A feature a caller may require of a deployment."""

    STREAMING = "streaming"
    TOOL_CALLING = "tool_calling"
    VISION = "vision"
    EMBEDDINGS = "embeddings"


@dataclass(frozen=True, slots=True)
class RoutingKey:
    """Identifies a servable target as (base model, adapter).

    ``adapter_id`` is empty for a plain base model. This is the routing table's
    key from the first release rather than a bare model name, because
    multi-LoRA serving means one base deployment serves many adapters — and
    retrofitting the key later is a migration of the catalog, the snapshot and
    every consumer.

    Frozen so it can be a dictionary key and cannot be mutated after a builder
    has indexed by it.
    """

    base_model: str
    adapter_id: str = ""

    def __post_init__(self) -> None:
        if not self.base_model:
            raise InvalidRequestError("a routing key needs a base model")

    def __str__(self) -> str:
        return f"{self.base_model}+{self.adapter_id}" if self.adapter_id else self.base_model


@dataclass(frozen=True, slots=True)
class Cost:
    """Price per thousand tokens, in millionths of a US dollar.

    Integer, never float: per-token prices are small enough that float rounding
    reaches the invoice.
    """

    input_per_1k_micro_usd: int = 0
    output_per_1k_micro_usd: int = 0

    def __post_init__(self) -> None:
        if self.input_per_1k_micro_usd < 0 or self.output_per_1k_micro_usd < 0:
            raise InvalidRequestError("a price cannot be negative")


@dataclass(frozen=True, slots=True, kw_only=True)
class Deployment:
    """One reachable endpoint that can serve a routing key.

    Keyword-only because a positional constructor with this many same-typed
    string fields is unreadable and fragile to reordering.
    """

    id: str
    key: RoutingKey
    provider: str
    endpoint: str
    region: str = ""
    trust_tier: TrustTier = TrustTier.UNSET
    # A reference to a secret, never the secret. Credentials must not enter a
    # snapshot: snapshots are replicated to every worker, cached and versioned.
    credential_ref: str = ""
    # Share of traffic, 0-100. Zero means registered but not serving — where a
    # fine-tuned adapter sits while it takes shadow traffic.
    weight: int = 0
    cost: Cost = field(default_factory=Cost)
    capabilities: tuple[Capability, ...] = ()

    def __post_init__(self) -> None:
        if not self.id:
            raise InvalidRequestError("a deployment needs an id")
        if not self.provider:
            raise InvalidRequestError(f"deployment {self.id!r} names no provider")
        if self.trust_tier is TrustTier.UNSET:
            raise InvalidRequestError(f"deployment {self.id!r} has an unset trust tier")
        if not 0 <= self.weight <= 100:
            raise InvalidRequestError(
                f"deployment {self.id!r} weight {self.weight} is out of range"
            )


@dataclass(frozen=True, slots=True)
class ModelAlias:
    """Decouples client code from concrete model ids.

    Callers ask for ``fast`` or ``reasoning``; the snapshot decides what that
    means today. Targets are in preference order.
    """

    name: str
    targets: tuple[RoutingKey, ...]

    def __post_init__(self) -> None:
        if not self.name:
            raise InvalidRequestError("an alias needs a name")
        if not self.targets:
            raise InvalidRequestError(f"alias {self.name!r} has no targets")
