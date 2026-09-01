"""Compile the domain model into the snapshot the data plane serves from.

This is the control plane's whole reason for existing: it turns a relational
identity graph, a model catalog and a set of budgets into an immutable,
versioned artifact that a worker can answer every request from without touching
a database.

Two things make this file worth reading carefully.

**It produces bytes another language parses.** Everything here has an exact
counterpart in the Go data plane's ``internal/wire``. A field written here and
read differently there is a class of bug that no unit test on either side alone
can catch, which is why the schema is generated for both from one ``.proto``
and why the digest is computed identically.

**The principal is flattened here, once.** The org/team/user/app graph is walked
at build time so that the data plane resolves a key in a single hash lookup. Any
work that stays in the request path is work multiplied by every request forever.
"""

from __future__ import annotations

import hashlib
from datetime import UTC, datetime

from model_gateway_control.domain.budget import Budget, BudgetScope
from model_gateway_control.domain.catalog import (
    Cost,
    Deployment,
    ModelAlias,
    RoutingKey,
    TrustTier,
)
from model_gateway_control.domain.identity import BudgetRef, Principal
from model_gateway_control.domain.tenant import Fleet, PluginBinding, Tenant
from model_gateway_control.errors import InvalidRequestError
from model_gateway_control.wire import snapshot_pb2 as pb

#: Names the hash, so the format can change later without every stored digest
#: becoming ambiguous. Must match the Go side's ``wire.DigestPrefix``.
DIGEST_PREFIX = "sha256:"

_PORTS = {
    "provider": pb.PORT_PROVIDER,
    "guardrail": pb.PORT_GUARDRAIL,
    "store": pb.PORT_STORE,
    "telemetry": pb.PORT_TELEMETRY,
}

_TRUST_TIERS = {
    TrustTier.EXTERNAL: pb.TRUST_TIER_EXTERNAL,
    TrustTier.PRIVATE_CLOUD: pb.TRUST_TIER_PRIVATE_CLOUD,
    TrustTier.INTERNAL: pb.TRUST_TIER_INTERNAL,
}

_BUDGET_SCOPES = {
    BudgetScope.KEY: pb.BUDGET_SCOPE_KEY,
    BudgetScope.APP: pb.BUDGET_SCOPE_APP,
    BudgetScope.USER: pb.BUDGET_SCOPE_USER,
    BudgetScope.TEAM: pb.BUDGET_SCOPE_TEAM,
    BudgetScope.ORG: pb.BUDGET_SCOPE_ORG,
    BudgetScope.MODEL: pb.BUDGET_SCOPE_MODEL,
    BudgetScope.TRAINING: pb.BUDGET_SCOPE_TRAINING,
}


def build_snapshot(fleet: Fleet, tenants: list[Tenant], built_at: datetime) -> pb.Snapshot:
    """Compile a full snapshot and seal every layer with its digest.

    Validation happens before anything is encoded, so a snapshot that exists is
    coherent and the data plane's own validation never has to be the first line
    of defence.
    """
    _validate(fleet, tenants)

    message = pb.Snapshot()
    message.global_layer.CopyFrom(encode_fleet(fleet, tenants, built_at))
    for tenant in tenants:
        message.tenants.append(encode_tenant(tenant, built_at))

    seal(message.global_layer)
    for layer in message.tenants:
        seal(layer)
    return message


def _validate(fleet: Fleet, tenants: list[Tenant]) -> None:
    """Reject a configuration that would build cleanly and then misbehave."""
    routes = {d.key for d in fleet.deployments}

    seen_ids: set[str] = set()
    for deployment in fleet.deployments:
        if deployment.id in seen_ids:
            raise InvalidRequestError(f"duplicate deployment id {deployment.id!r}")
        seen_ids.add(deployment.id)

    for alias in fleet.aliases:
        for target in alias.targets:
            if target not in routes:
                # A dangling alias is a 404 for a model an operator believes is
                # configured, and the failure surfaces only when someone calls
                # it.
                raise InvalidRequestError(
                    f"alias {alias.name!r} targets {target}, which has no deployment"
                )

    seen_prefixes: dict[str, str] = {}
    seen_tenants: set[str] = set()
    for tenant in tenants:
        if tenant.id in seen_tenants:
            raise InvalidRequestError(f"two layers for tenant {tenant.id!r}")
        seen_tenants.add(tenant.id)

        for prefix in tenant.key_prefixes:
            owner = seen_prefixes.get(prefix)
            if owner is not None:
                # Two tenants sharing a prefix means one of them silently
                # authenticates as the other.
                raise InvalidRequestError(
                    f"key prefix {prefix!r} is claimed by both {owner!r} and {tenant.id!r}"
                )
            seen_prefixes[prefix] = tenant.id

        _validate_tenant(tenant)


def _validate_tenant(tenant: Tenant) -> None:
    budget_ids = {b.id for b in tenant.budgets}
    principal_ids = {p.key_id for p in tenant.principals}

    for principal in tenant.principals:
        if principal.tenant != tenant.id:
            raise InvalidRequestError(
                f"principal {principal.key_id!r} belongs to {principal.tenant!r}, not {tenant.id!r}"
            )
        for ref in principal.budgets:
            if ref.id not in budget_ids:
                # A dangling budget reference makes admission silently skip a
                # limit an operator believes is enforced.
                raise InvalidRequestError(
                    f"principal {principal.key_id!r} references unknown budget {ref.id!r}"
                )

    for lookup, key_id in tenant.keys.items():
        if len(lookup) != hashlib.sha256().digest_size:
            raise InvalidRequestError(f"tenant {tenant.id!r} has a malformed key lookup")
        if key_id not in principal_ids:
            raise InvalidRequestError(
                f"tenant {tenant.id!r} maps a key to unknown principal {key_id!r}"
            )


def encode_fleet(fleet: Fleet, tenants: list[Tenant], built_at: datetime) -> pb.GlobalLayer:
    """Encode the tenant-independent layer."""
    layer = pb.GlobalLayer(
        version=pb.LayerVersion(number=fleet.version),
        built_at_unix_ms=_to_unix_ms(built_at),
        policy_bundle_ref=fleet.policy_bundle_ref,
    )
    for deployment in fleet.deployments:
        layer.deployments.append(_encode_deployment(deployment))
    for alias in fleet.aliases:
        layer.aliases.append(_encode_alias(alias))
    for binding in fleet.default_plugins:
        layer.default_plugins.append(_encode_plugin(binding))

    # The prefix map lives in the global layer because resolving a key must
    # find its tenant before any tenant layer is consulted.
    for tenant in tenants:
        for prefix in tenant.key_prefixes:
            layer.tenant_prefixes[prefix] = tenant.id
    return layer


def encode_tenant(tenant: Tenant, built_at: datetime) -> pb.TenantLayer:
    """Encode one tenant's layer."""
    layer = pb.TenantLayer(
        tenant=tenant.id,
        version=pb.LayerVersion(number=tenant.version),
        built_at_unix_ms=_to_unix_ms(built_at),
        tier=tenant.tier,
        min_trust_tier=_TRUST_TIERS.get(tenant.min_trust_tier, pb.TRUST_TIER_UNSPECIFIED),
    )
    for principal in tenant.principals:
        layer.principals.append(_encode_principal(principal))
    # Sorted so that the same tenant produces the same bytes and therefore the
    # same digest on every build, independent of dictionary ordering.
    for lookup in sorted(tenant.keys):
        layer.keys.append(pb.KeyEntry(lookup=lookup, key_id=tenant.keys[lookup]))
    for alias in tenant.alias_overrides:
        layer.alias_overrides.append(_encode_alias(alias))
    for budget in tenant.budgets:
        layer.budgets.append(_encode_budget(budget))
    for binding in tenant.plugins:
        layer.plugins.append(_encode_plugin(binding))
    return layer


def _encode_deployment(d: Deployment) -> pb.Deployment:
    return pb.Deployment(
        id=d.id,
        key=_encode_routing_key(d.key),
        provider=d.provider,
        endpoint=d.endpoint,
        region=d.region,
        trust_tier=_TRUST_TIERS.get(d.trust_tier, pb.TRUST_TIER_UNSPECIFIED),
        credential_ref=d.credential_ref,
        weight=d.weight,
        cost=_encode_cost(d.cost),
        capabilities=[str(c) for c in d.capabilities],
    )


def _encode_routing_key(k: RoutingKey) -> pb.RoutingKey:
    return pb.RoutingKey(base_model=k.base_model, adapter_id=k.adapter_id)


def _encode_cost(c: Cost) -> pb.Cost:
    return pb.Cost(
        input_per_1k_micro_usd=c.input_per_1k_micro_usd,
        output_per_1k_micro_usd=c.output_per_1k_micro_usd,
        cached_input_per_1k_micro_usd=c.cached_input_per_1k_micro_usd,
        cache_write_per_1k_micro_usd=c.cache_write_per_1k_micro_usd,
    )


def _encode_alias(a: ModelAlias) -> pb.ModelAlias:
    return pb.ModelAlias(name=a.name, targets=[_encode_routing_key(t) for t in a.targets])


def _encode_plugin(b: PluginBinding) -> pb.PluginBinding:
    return pb.PluginBinding(
        port=_PORTS.get(b.port, pb.PORT_UNSPECIFIED),
        component=b.component,
        version=b.version,
        config_ref=b.config_ref,
    )


def _encode_budget(b: Budget) -> pb.BudgetState:
    return pb.BudgetState(
        id=b.id,
        scope=_BUDGET_SCOPES.get(b.scope, pb.BUDGET_SCOPE_UNSPECIFIED),
        limit_micro_usd=b.limit_micro_usd,
        spent_micro_usd=b.spent_micro_usd,
        hard=b.hard,
        headroom_basis_points=b.headroom_basis_points,
    )


def _encode_budget_ref(r: BudgetRef) -> pb.BudgetRef:
    return pb.BudgetRef(id=r.id, scope=_BUDGET_SCOPES.get(r.scope, pb.BUDGET_SCOPE_UNSPECIFIED))


def _encode_principal(p: Principal) -> pb.Principal:
    return pb.Principal(
        key_id=p.key_id,
        tenant=p.tenant,
        org=p.org,
        team=p.team,
        user=p.user,
        app=p.app,
        roles=list(p.roles),
        models_allow_all=p.models_allow_all,
        models=list(p.models),
        budgets=[_encode_budget_ref(r) for r in p.budgets],
        default_data_class=p.default_data_class,
        min_trust_tier=_TRUST_TIERS.get(p.min_trust_tier, pb.TRUST_TIER_UNSPECIFIED),
        # max_concurrent is also written at its original tag so that a worker
        # built before RateLimit existed still sees the limit. A field tag is
        # never reused, and an old reader is a normal state during a rollout.
        max_concurrent=p.limits.max_concurrent,
        limits=pb.RateLimit(
            requests_per_minute=p.limits.requests_per_minute,
            tokens_per_minute=p.limits.tokens_per_minute,
            max_concurrent=p.limits.max_concurrent,
        ),
        deprecated=p.deprecated,
        not_after_unix_ms=_to_unix_ms(p.not_after) if p.not_after else 0,
    )


def _to_unix_ms(when: datetime) -> int:
    """Convert to Unix milliseconds, treating a naive datetime as UTC.

    ``datetime.timestamp()`` on a naive value assumes *local* time, so a key
    expiry read back from a driver that drops timezones would shift by the
    server's UTC offset — a key expiring hours early or late depending on where
    the process runs. Everything stored is UTC, so saying so here is the correct
    reading rather than a guess.
    """
    if when.tzinfo is None:
        when = when.replace(tzinfo=UTC)
    return int(when.timestamp() * 1000)


# --- digests ----------------------------------------------------------------
#
# A layer's digest content-addresses it, so a worker can skip a re-fetch it
# already holds and two workers claiming a version can be proven to agree.
#
# It is computed over the layer with its own digest field cleared, since a field
# cannot cover itself, and over deterministically serialized bytes: protobuf map
# ordering is arbitrary by default, so a non-deterministic digest would differ
# between the producer and the verifier and reject layers at random.


def seal[Layer: (pb.GlobalLayer, pb.TenantLayer)](layer: Layer) -> str:
    """Stamp a layer with its own digest and return it.

    One function for both layer kinds: they differ in content, not in how they
    are addressed, and two near-identical copies would be two places to get the
    clearing step wrong.
    """
    copy = type(layer)()
    copy.CopyFrom(layer)
    copy.version.ClearField("digest")

    digest = DIGEST_PREFIX + hashlib.sha256(copy.SerializeToString(deterministic=True)).hexdigest()
    layer.version.digest = digest
    return digest
