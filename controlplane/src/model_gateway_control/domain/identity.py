"""The org -> team -> user -> application -> API key hierarchy.

The graph is stored as a **materialized closure table** rather than walked with
recursive CTEs, and it is flattened into a ``Principal`` here so that the data
plane resolves a key in one hash lookup. The whole point of the control plane
owning this is that the request path never sees a graph at all.
"""

from __future__ import annotations

import hashlib
import hmac
import secrets
from dataclasses import dataclass, field
from datetime import datetime

from model_gateway_control.domain.budget import BudgetScope
from model_gateway_control.domain.catalog import TrustTier
from model_gateway_control.errors import InvalidRequestError

#: Bytes of randomness in a key secret. 32 bytes is 256 bits, which is why the
#: lookup below is a keyed hash rather than a password KDF: there is nothing to
#: brute-force, so Argon2id's deliberate 10-100 ms would buy nothing and cost
#: the data plane its entire latency budget.
KEY_SECRET_BYTES = 32

#: Minimum pepper length. A short pepper is worse than a missing one, because
#: it looks configured.
MIN_PEPPER_BYTES = 32

KEY_SCHEME = "gw_"


@dataclass(frozen=True, slots=True)
class DataClass:
    """Placeholder for the sensitivity vocabulary, kept as data.

    The classification-to-trust-tier mapping is tenant policy and lives in the
    snapshot, not in code.
    """

    name: str


@dataclass(frozen=True, slots=True)
class BudgetRef:
    """One link in a principal's budget chain."""

    id: str
    scope: BudgetScope


@dataclass(frozen=True, slots=True)
class RateLimit:
    """What a principal may consume per minute.

    Zero means unlimited for that dimension. That is the only safe default: a
    principal that predates a limit must not suddenly be capped at zero, which
    would make adding a field an outage.

    All three are approximate by construction — requests are leased in blocks,
    tokens are counted from usage reported after the fact, and concurrency is
    per worker. Budgets are the mechanism for anything that must be exact, and
    they are separate for precisely this reason.
    """

    requests_per_minute: int = 0
    tokens_per_minute: int = 0
    max_concurrent: int = 0

    def __post_init__(self) -> None:
        if min(self.requests_per_minute, self.tokens_per_minute, self.max_concurrent) < 0:
            raise InvalidRequestError("a rate limit cannot be negative")


@dataclass(frozen=True, slots=True, kw_only=True)
class Principal:
    """The precomputed record a key resolves to.

    Every ancestor, the effective role set, the model allowlist and the whole
    budget chain are folded in here by the builder, so admission never walks the
    identity graph at request time.
    """

    key_id: str
    tenant: str
    org: str = ""
    team: str = ""
    user: str = ""
    app: str = ""

    roles: tuple[str, ...] = ()
    #: ``allow_all`` is explicit rather than implied by an empty list, so that
    #: an empty allowlist means "nothing" and a builder that forgets to populate
    #: it fails closed.
    models_allow_all: bool = False
    models: tuple[str, ...] = ()
    budgets: tuple[BudgetRef, ...] = ()

    default_data_class: str = ""
    min_trust_tier: TrustTier = TrustTier.UNSET
    limits: RateLimit = field(default_factory=RateLimit)

    #: The outgoing generation of a rotated key stays valid until ``not_after``,
    #: which is what makes rotation a non-event for callers.
    deprecated: bool = False
    not_after: datetime | None = None

    def __post_init__(self) -> None:
        if not self.key_id:
            raise InvalidRequestError("a principal needs a key id")
        if not self.tenant:
            raise InvalidRequestError(f"principal {self.key_id!r} names no tenant")


@dataclass(frozen=True, slots=True)
class IssuedKey:
    """A freshly minted key: the only moment the secret exists in clear text.

    ``presented`` is returned to the operator once and never stored. What the
    control plane persists is ``lookup``, which is useless without the pepper.
    """

    key_id: str
    tenant_prefix: str
    presented: str
    lookup: bytes = field(repr=False)

    def __repr__(self) -> str:
        # Never let a secret reach a log through a stack trace or a debug print.
        return (
            f"IssuedKey(key_id={self.key_id!r}, "
            f"tenant_prefix={self.tenant_prefix!r}, presented=<redacted>)"
        )


def compute_key_lookup(pepper: bytes, secret: str) -> bytes:
    """Derive the value a snapshot indexes a principal by.

    HMAC-SHA256 under a server-held pepper: constant-time to compare, and a
    stolen database yields nothing without the pepper. This must produce exactly
    the same bytes as the Go data plane's ``core.ComputeKeyLookup``; the
    round-trip test in the builder suite is what keeps that true.
    """
    if len(pepper) < MIN_PEPPER_BYTES:
        raise InvalidRequestError(
            f"the key pepper must be at least {MIN_PEPPER_BYTES} bytes, got {len(pepper)}"
        )
    return hmac.new(pepper, secret.encode(), hashlib.sha256).digest()


def issue_key(pepper: bytes, tenant_prefix: str, key_id: str) -> IssuedKey:
    """Mint a new API key.

    The prefix is not a secret. It exists so a presented key resolves to exactly
    one tenant layer before any cryptographic work happens, which is what keeps
    lookup a single hash probe rather than a scan across every tenant.
    """
    if not tenant_prefix or "_" in tenant_prefix:
        raise InvalidRequestError("a tenant prefix must be non-empty and contain no underscore")
    if not key_id:
        raise InvalidRequestError("an issued key needs an id")

    secret = secrets.token_urlsafe(KEY_SECRET_BYTES)
    return IssuedKey(
        key_id=key_id,
        tenant_prefix=tenant_prefix,
        presented=f"{KEY_SCHEME}{tenant_prefix}_{secret}",
        lookup=compute_key_lookup(pepper, secret),
    )
