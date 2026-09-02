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
from collections.abc import Sequence
from datetime import UTC, datetime

from model_gateway_control.domain.budget import Budget, BudgetScope
from model_gateway_control.domain.catalog import (
    Cost,
    Deployment,
    ModelAlias,
    RoutingKey,
    TrustTier,
)
from model_gateway_control.domain.component import Component, Port, Registry
from model_gateway_control.domain.identity import BudgetRef, Principal
from model_gateway_control.domain.policy import PolicyBundle, PolicyEffect
from model_gateway_control.domain.signing import TrustStore
from model_gateway_control.domain.tenant import (
    FailureMode,
    Fleet,
    GuardrailBinding,
    PluginBinding,
    Tenant,
)
from model_gateway_control.errors import GatewayError, InvalidRequestError
from model_gateway_control.wire import snapshot_pb2 as pb

#: Names the hash, so the format can change later without every stored digest
#: becoming ambiguous. Must match the Go side's ``wire.DigestPrefix``.
DIGEST_PREFIX = "sha256:"

#: Only the data-plane ports have a wire representation. A control-plane
#: component is governed by the same registry but never reaches a worker, so a
#: binding for one is a build error rather than a field nothing reads.
_PORTS = {
    Port.PROVIDER: pb.PORT_PROVIDER,
    Port.GUARDRAIL: pb.PORT_GUARDRAIL,
    Port.STORE: pb.PORT_STORE,
    Port.TELEMETRY: pb.PORT_TELEMETRY,
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


def build_snapshot(
    fleet: Fleet,
    tenants: list[Tenant],
    built_at: datetime,
    trust: TrustStore | None = None,
) -> pb.Snapshot:
    """Compile a full snapshot and seal every layer with its digest.

    Validation happens before anything is encoded, so a snapshot that exists is
    coherent and the data plane's own validation never has to be the first line
    of defence.

    ``trust`` is where publisher signatures are actually enforced. Registration
    checks them too, but that check leaves behind a row, and a row is what an
    attacker with a database has; this one re-derives the answer from keys that
    live in configuration. Defaulting to an empty store means an unsigned
    component still builds, while a *signed* one fails — loudly — because a
    signature nobody can check must never pass for a valid one.
    """
    trust = trust or TrustStore()
    _validate(fleet, tenants, trust)

    message = pb.Snapshot()
    message.global_layer.CopyFrom(encode_fleet(fleet, tenants, built_at))
    for tenant in tenants:
        message.tenants.append(encode_tenant(tenant, built_at, fleet.registry))

    seal(message.global_layer)
    for layer in message.tenants:
        seal(layer)
    return message


def _validate(fleet: Fleet, tenants: list[Tenant], trust: TrustStore) -> None:
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
        _validate_bindings(fleet.registry, tenant.id, tenant.plugins, tenant.guardrails, trust)

    _validate_bindings(fleet.registry, None, fleet.default_plugins, fleet.default_guardrails, trust)


def _validate_bindings(
    registry: Registry,
    tenant: str | None,
    plugins: Sequence[PluginBinding],
    guardrails: Sequence[GuardrailBinding],
    trust: TrustStore,
) -> None:
    """Reject a binding the registry cannot vouch for.

    This is the admission gate's only teeth. Without it the registry is a table
    nobody consults: a binding naming a component that was never registered,
    never passed its suite, or has since been retired would compile into a
    snapshot and reach every worker, where the failure is a warning in a log at
    request time — for a control an operator believes is enforcing.
    """
    where = f"tenant {tenant!r}" if tenant is not None else "the fleet defaults"

    for plugin in plugins:
        component = _resolve(registry, plugin.port, plugin.component, plugin.version, where)
        _require_bindable(component, where)
        _require_signature(component, where, trust)

    for guardrail in guardrails:
        component = _resolve(
            registry, Port.GUARDRAIL, guardrail.component, guardrail.version, where
        )
        _require_bindable(component, where)
        _require_signature(component, where, trust)

        declared = component.manifest.latency_budget_ms
        if declared and guardrail.timeout_ms < declared:
            # The binding would cut the component off before it could finish,
            # on every request. Which way that fails depends on the failure
            # mode, and both ways are wrong.
            raise InvalidRequestError(
                f"{where} allows {component.manifest.ref} {guardrail.timeout_ms}ms, "
                f"but it declares it needs {declared}ms"
            )


def _resolve(registry: Registry, port: Port, component: str, version: str, where: str) -> Component:
    try:
        return registry.resolve(port, component, version)
    except GatewayError as exc:
        raise InvalidRequestError(f"{where}: {exc.message}") from exc


def _require_bindable(component: Component, where: str) -> None:
    if component.is_bindable:
        return
    raise InvalidRequestError(
        f"{where} binds {component.manifest.ref}, which is {component.status} — "
        f"only an admitted, active component can enter a snapshot"
    )


def _require_signature(component: Component, where: str, trust: TrustStore) -> None:
    """Re-derive whether a component's publisher signature holds.

    Not a lookup of what registration decided. Registration's answer lives in
    the database, and so does the status that says a component is bindable; if
    both came from there, an attacker who could write one could write the
    other. This recomputes the signature against keys that came from
    configuration, so getting a forged component into a snapshot takes the
    trusted-keys file and not just the database.

    It is also where revocation takes effect: a key revoked because it is
    believed compromised stops verifying, so its components leave the fleet on
    the next snapshot rather than whenever someone remembers to retire them.
    """
    try:
        trust.verify(component.manifest.digest(), component.signature)
    except GatewayError as exc:
        raise InvalidRequestError(f"{where} binds {component.manifest.ref}: {exc.message}") from exc


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
    for guardrail in fleet.default_guardrails:
        layer.default_guardrails.append(_encode_guardrail(guardrail, fleet.registry))
    if fleet.default_policy is not None:
        layer.default_policy.CopyFrom(_encode_policy(fleet.default_policy))

    # The prefix map lives in the global layer because resolving a key must
    # find its tenant before any tenant layer is consulted.
    for tenant in tenants:
        for prefix in tenant.key_prefixes:
            layer.tenant_prefixes[prefix] = tenant.id
    return layer


def encode_tenant(tenant: Tenant, built_at: datetime, registry: Registry) -> pb.TenantLayer:
    """Encode one tenant's layer.

    The registry is needed to say how each bound guardrail runs; see
    _encode_guardrail for why that travels in the snapshot.
    """
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
    for guardrail in tenant.guardrails:
        layer.guardrails.append(_encode_guardrail(guardrail, registry))
    if tenant.policy is not None:
        layer.policy.CopyFrom(_encode_policy(tenant.policy))
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
        shadow_percent=d.shadow_percent,
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
    port = _PORTS.get(b.port)
    if port is None:
        # Defaulting to PORT_UNSPECIFIED would ship a binding the worker
        # silently ignores, which is the failure this whole module removes.
        raise InvalidRequestError(f"{b.port} components do not run in the data plane")
    return pb.PluginBinding(
        port=port,
        component=b.component,
        version=b.version,
        config_ref=b.config_ref,
    )


_POLICY_EFFECTS = {
    PolicyEffect.ALLOW: pb.POLICY_EFFECT_ALLOW,
    PolicyEffect.DENY: pb.POLICY_EFFECT_DENY,
}


def _encode_policy(bundle: PolicyBundle) -> pb.PolicyBundle:
    """Compile a bundle into the form the data plane evaluates.

    Compilation is the whole point: the worker receives an ordered table and
    scans it, rather than parsing rules on every request. Everything that can
    be decided once — rule order, condition sets, network validity — is decided
    here.
    """
    encoded = pb.PolicyBundle(
        id=bundle.id,
        version=bundle.version,
        default_effect=_POLICY_EFFECTS[bundle.default_effect],
    )
    for rule in bundle.rules:
        encoded.rules.append(
            pb.PolicyRule(
                id=rule.id,
                effect=_POLICY_EFFECTS[rule.effect],
                models=list(rule.models),
                endpoints=list(rule.endpoints),
                roles=list(rule.roles),
                regions=list(rule.regions),
                source_cidrs=list(rule.source_cidrs),
                max_payload_bytes=rule.max_payload_bytes,
                data_class=rule.data_class,
                min_trust_tier=_TRUST_TIERS.get(rule.min_trust_tier, pb.TRUST_TIER_UNSPECIFIED),
                reason=rule.reason,
            )
        )
    return encoded


_FAILURE_MODES = {
    FailureMode.OPEN: pb.FAILURE_MODE_OPEN,
    FailureMode.CLOSED: pb.FAILURE_MODE_CLOSED,
}


def _encode_guardrail(g: GuardrailBinding, registry: Registry) -> pb.GuardrailBinding:
    # How the worker must run this component travels with the binding, because
    # the snapshot is the worker's only source of configuration. A worker that
    # had to ask the control plane how to run something it was already told to
    # run would stop serving when the control plane was down.
    #
    # The registry has already vouched for every binding by this point, so the
    # lookup cannot fail; it is done here rather than carried on the domain
    # binding because the answer belongs to the component, not to the tenant
    # that bound it.
    manifest = registry.resolve(Port.GUARDRAIL, g.component, g.version).manifest

    return pb.GuardrailBinding(
        component=g.component,
        version=g.version,
        config_ref=g.config_ref,
        execution=str(manifest.execution),
        module=manifest.module,
        timeout_ms=g.timeout_ms,
        # An unmapped mode encodes as closed rather than unspecified. The data
        # plane reads an unrecognised mode as fail-closed too, so the safe
        # reading of "I do not know what this control does" is consistent on
        # both sides.
        failure_mode=_FAILURE_MODES.get(g.failure_mode, pb.FAILURE_MODE_CLOSED),
        blocking=g.blocking,
        phases=list(g.phases),
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
