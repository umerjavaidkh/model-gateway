"""The builder produces the artifact the Go data plane serves from.

The assertions here are about *shape and invariants*. Whether the Go side reads
the same bytes back is proven separately, in test_cross_language.py, because no
amount of Python-side testing can establish that.
"""

from __future__ import annotations

import dataclasses

import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat

from model_gateway_control.domain.budget import Budget, BudgetScope
from model_gateway_control.domain.catalog import ModelAlias, RoutingKey, TrustTier
from model_gateway_control.domain.component import (
    Component,
    Execution,
    Manifest,
    Port,
    Registry,
    Status,
    admitted,
)
from model_gateway_control.domain.identity import BudgetRef, Principal
from model_gateway_control.domain.signing import (
    KeyStatus,
    PublisherKey,
    TrustStore,
    sign,
)
from model_gateway_control.domain.tenant import Fleet, Tenant
from model_gateway_control.errors import InvalidRequestError
from model_gateway_control.snapshot import build_snapshot, seal
from tests.conftest import BUILT_AT, ROUTE_GPT, ROUTE_LLAMA, guardrail_component


def test_snapshot_carries_both_layers(fleet: Fleet, tenant: Tenant) -> None:
    snapshot = build_snapshot(fleet, [tenant], BUILT_AT)

    assert snapshot.global_layer.version.number == 7
    assert len(snapshot.global_layer.deployments) == 3
    assert len(snapshot.tenants) == 1
    assert snapshot.tenants[0].tenant == "acme"
    assert snapshot.tenants[0].version.number == 3


def test_key_prefixes_are_folded_into_the_global_layer(fleet: Fleet, tenant: Tenant) -> None:
    # Resolving a key must find its tenant before any tenant layer is consulted,
    # which is why the map lives in the global half.
    snapshot = build_snapshot(fleet, [tenant], BUILT_AT)
    assert dict(snapshot.global_layer.tenant_prefixes) == {"acme": "acme"}


def test_every_layer_is_sealed(fleet: Fleet, tenant: Tenant) -> None:
    snapshot = build_snapshot(fleet, [tenant], BUILT_AT)

    assert snapshot.global_layer.version.digest.startswith("sha256:")
    assert snapshot.tenants[0].version.digest.startswith("sha256:")
    assert snapshot.global_layer.version.digest != snapshot.tenants[0].version.digest


def test_the_same_input_produces_the_same_digest(fleet: Fleet, tenant: Tenant) -> None:
    # Protobuf map ordering is arbitrary by default, so a non-deterministic
    # digest would differ between producer and verifier and reject layers at
    # random.
    first = build_snapshot(fleet, [tenant], BUILT_AT)
    second = build_snapshot(fleet, [tenant], BUILT_AT)

    assert first.global_layer.version.digest == second.global_layer.version.digest
    assert first.tenants[0].version.digest == second.tenants[0].version.digest
    assert first.SerializeToString(deterministic=True) == second.SerializeToString(
        deterministic=True
    )


def test_the_digest_covers_the_payload_not_just_the_header(fleet: Fleet, tenant: Tenant) -> None:
    before = build_snapshot(fleet, [tenant], BUILT_AT).global_layer.version.digest

    changed = dataclasses.replace(
        fleet,
        deployments=(
            dataclasses.replace(fleet.deployments[0], weight=50),
            *fleet.deployments[1:],
        ),
    )
    after = build_snapshot(changed, [tenant], BUILT_AT).global_layer.version.digest

    assert before != after


def test_sealing_twice_is_stable(fleet: Fleet, tenant: Tenant) -> None:
    # The digest is computed with the digest field cleared, so re-sealing must
    # not hash the previous hash.
    snapshot = build_snapshot(fleet, [tenant], BUILT_AT)
    first = snapshot.global_layer.version.digest
    assert seal(snapshot.global_layer) == first


def test_keys_are_emitted_in_a_stable_order(fleet: Fleet) -> None:
    # Dictionary ordering must not reach the digest, or the same configuration
    # produces a different artifact on every build.
    from model_gateway_control.domain.identity import compute_key_lookup
    from tests.conftest import PEPPER

    principals = tuple(
        Principal(key_id=f"key-{i}", tenant="acme", models_allow_all=True) for i in range(20)
    )
    keys = {compute_key_lookup(PEPPER, f"secret-{i}"): f"key-{i}" for i in range(20)}
    tenant = Tenant(id="acme", key_prefixes=("acme",), principals=principals, keys=keys)

    first = build_snapshot(fleet, [tenant], BUILT_AT)
    second = build_snapshot(fleet, [tenant], BUILT_AT)
    assert first.tenants[0].version.digest == second.tenants[0].version.digest


def test_tenant_alias_overrides_are_kept_separate(fleet: Fleet, tenant: Tenant) -> None:
    # The layering exists so the same global catalog means different things per
    # tenant. Merging them at build time would throw that away.
    snapshot = build_snapshot(fleet, [tenant], BUILT_AT)

    global_fast = {a.name: a for a in snapshot.global_layer.aliases}["fast"]
    tenant_fast = {a.name: a for a in snapshot.tenants[0].alias_overrides}["fast"]

    assert global_fast.targets[0].base_model == ROUTE_GPT.base_model
    assert tenant_fast.targets[0].base_model == ROUTE_LLAMA.base_model


def test_adapter_is_a_separate_routing_target(fleet: Fleet, tenant: Tenant) -> None:
    snapshot = build_snapshot(fleet, [tenant], BUILT_AT)
    by_id = {d.id: d for d in snapshot.global_layer.deployments}

    assert by_id["vllm-1"].key.adapter_id == ""
    assert by_id["vllm-1-triage"].key.adapter_id == "triage-v3"
    # A pre-canary adapter is registered but not serving.
    assert by_id["vllm-1-triage"].weight == 0


def test_credentials_are_references_never_secrets(fleet: Fleet, tenant: Tenant) -> None:
    # A snapshot is replicated to every worker, cached and versioned. Anything
    # secret-shaped in it is multiplied across the fleet and outlives rotation.
    snapshot = build_snapshot(fleet, [tenant], BUILT_AT)
    serialized = snapshot.SerializeToString()

    by_id = {d.id: d for d in snapshot.global_layer.deployments}
    assert by_id["openai-1"].credential_ref == "env:OPENAI_API_KEY"
    assert b"sk-" not in serialized


def test_a_dangling_alias_is_rejected(fleet: Fleet, tenant: Tenant) -> None:
    # A 404 for a model an operator believes is configured, surfacing only when
    # someone calls it.
    broken = dataclasses.replace(
        fleet, aliases=(ModelAlias(name="ghost", targets=(RoutingKey(base_model="nope"),)),)
    )
    with pytest.raises(InvalidRequestError, match="no deployment"):
        build_snapshot(broken, [tenant], BUILT_AT)


def test_a_dangling_budget_reference_is_rejected(fleet: Fleet, tenant: Tenant) -> None:
    # Admission would silently skip a limit an operator believes is enforced.
    broken = dataclasses.replace(
        tenant,
        principals=(
            dataclasses.replace(
                tenant.principals[0], budgets=(BudgetRef(id="ghost", scope=BudgetScope.ORG),)
            ),
        ),
    )
    with pytest.raises(InvalidRequestError, match="unknown budget"):
        build_snapshot(fleet, [broken], BUILT_AT)


def test_two_tenants_cannot_share_a_key_prefix(fleet: Fleet, tenant: Tenant) -> None:
    # One of them would silently authenticate as the other.
    other = Tenant(id="globex", key_prefixes=("acme",))
    with pytest.raises(InvalidRequestError, match="claimed by both"):
        build_snapshot(fleet, [tenant, other], BUILT_AT)


def test_a_duplicate_deployment_id_is_rejected(fleet: Fleet, tenant: Tenant) -> None:
    broken = dataclasses.replace(fleet, deployments=(*fleet.deployments, fleet.deployments[0]))
    with pytest.raises(InvalidRequestError, match="duplicate deployment"):
        build_snapshot(broken, [tenant], BUILT_AT)


def test_a_principal_from_another_tenant_is_rejected(fleet: Fleet, tenant: Tenant) -> None:
    broken = dataclasses.replace(
        tenant, principals=(dataclasses.replace(tenant.principals[0], tenant="globex"),)
    )
    with pytest.raises(InvalidRequestError, match="belongs to"):
        build_snapshot(fleet, [broken], BUILT_AT)


def test_a_key_mapping_to_no_principal_is_rejected(fleet: Fleet, tenant: Tenant) -> None:
    broken = dataclasses.replace(tenant, keys={b"x" * 32: "no-such-key"})
    with pytest.raises(InvalidRequestError, match="unknown principal"):
        build_snapshot(fleet, [broken], BUILT_AT)


def test_a_tenant_with_no_prefix_is_rejected() -> None:
    # It would build cleanly and then authenticate nobody.
    with pytest.raises(InvalidRequestError, match="no key prefix"):
        Tenant(id="acme")


def test_a_deployment_with_no_trust_tier_is_rejected() -> None:
    from model_gateway_control.domain.catalog import Deployment

    with pytest.raises(InvalidRequestError, match="unset trust tier"):
        Deployment(id="d", key=ROUTE_GPT, provider="p", endpoint="e", trust_tier=TrustTier.UNSET)


def test_budget_headroom_is_held_back() -> None:
    budget = Budget(
        id="b",
        scope=BudgetScope.ORG,
        limit_micro_usd=1_000_000,
        spent_micro_usd=940_000,
        headroom_basis_points=500,
    )
    assert budget.available_micro_usd == 10_000

    spent = dataclasses.replace(budget, spent_micro_usd=950_000)
    assert spent.available_micro_usd == 0


def test_token_class_rates_reach_the_snapshot(fleet: Fleet, tenant: Tenant) -> None:
    # A cache rate the worker never sees is a cache rate that does not apply,
    # and the request that would have used it is billed at the standard price.
    from model_gateway_control.domain.catalog import Cost

    priced = dataclasses.replace(
        fleet,
        deployments=(
            dataclasses.replace(
                fleet.deployments[0],
                cost=Cost(
                    input_per_1k_micro_usd=3000,
                    output_per_1k_micro_usd=15000,
                    cached_input_per_1k_micro_usd=300,
                    cache_write_per_1k_micro_usd=3750,
                ),
            ),
            *fleet.deployments[1:],
        ),
    )

    snapshot = build_snapshot(priced, [tenant], BUILT_AT)
    cost = snapshot.global_layer.deployments[0].cost

    assert cost.input_per_1k_micro_usd == 3000
    assert cost.cached_input_per_1k_micro_usd == 300
    assert cost.cache_write_per_1k_micro_usd == 3750


def test_unset_cache_rates_stay_zero_rather_than_defaulting_here() -> None:
    # The fallback to the standard input rate belongs in the data plane, at the
    # point of billing. Baking it in here would freeze today's rate into every
    # snapshot and make changing it a rebuild of history.
    from model_gateway_control.domain.catalog import Cost

    cost = Cost(input_per_1k_micro_usd=3000, output_per_1k_micro_usd=15000)
    assert cost.cached_input_per_1k_micro_usd == 0
    assert cost.cache_write_per_1k_micro_usd == 0


def test_guardrail_bindings_reach_the_snapshot(fleet: Fleet, tenant: Tenant) -> None:
    # A guardrail the worker never sees is a control that is not enforcing,
    # while an operator believes it is.
    from model_gateway_control.domain.tenant import FailureMode, GuardrailBinding

    registered = dataclasses.replace(
        fleet,
        registry=Registry(
            (
                guardrail_component("secret-scan", "1.0.0", latency_budget_ms=5),
                guardrail_component("injection-heuristics", "1.0.0"),
            )
        ),
        default_plugins=(),
    )
    guarded = dataclasses.replace(
        tenant,
        plugins=(),
        guardrails=(
            GuardrailBinding(
                component="secret-scan",
                timeout_ms=5,
                failure_mode=FailureMode.CLOSED,
                blocking=True,
            ),
            GuardrailBinding(
                component="injection-heuristics",
                timeout_ms=50,
                failure_mode=FailureMode.OPEN,
                blocking=False,
            ),
        ),
    )

    snapshot = build_snapshot(registered, [guarded], BUILT_AT)
    bindings = {g.component: g for g in snapshot.tenants[0].guardrails}

    assert bindings["secret-scan"].blocking is True
    assert bindings["secret-scan"].timeout_ms == 5
    assert bindings["injection-heuristics"].blocking is False


def test_how_a_guardrail_runs_travels_with_the_binding(fleet: Fleet, tenant: Tenant) -> None:
    # The snapshot is the worker's only source of configuration. A worker that
    # had to ask the control plane how to run something it was already told to
    # run would stop serving when the control plane was down.
    from model_gateway_control.domain.tenant import GuardrailBinding

    digest = "sha256:" + "a" * 64
    registered = dataclasses.replace(
        fleet,
        registry=Registry(
            (
                guardrail_component(
                    "wasm-guard",
                    "1.0.0",
                    execution=Execution.IN_PROCESS,
                    module=digest,
                ),
                guardrail_component("side-guard", "1.0.0"),
            )
        ),
        default_plugins=(),
    )
    guarded = dataclasses.replace(
        tenant,
        plugins=(),
        guardrails=(
            GuardrailBinding(component="wasm-guard", timeout_ms=50),
            GuardrailBinding(component="side-guard", timeout_ms=50),
        ),
    )

    snapshot = build_snapshot(registered, [guarded], BUILT_AT)
    bindings = {g.component: g for g in snapshot.tenants[0].guardrails}

    assert bindings["wasm-guard"].execution == "in_process"
    assert bindings["wasm-guard"].module == digest
    # A sidecar names no module, and a worker must not go looking for one.
    assert bindings["side-guard"].execution == "sidecar"
    assert bindings["side-guard"].module == ""


def test_a_guardrail_needs_a_positive_timeout() -> None:
    # A zero timeout is not "no limit"; the data plane would substitute its
    # default and the declared budget would be a fiction.
    from model_gateway_control.domain.tenant import GuardrailBinding

    with pytest.raises(InvalidRequestError, match="positive timeout"):
        GuardrailBinding(component="secret-scan", timeout_ms=0)


def test_an_unknown_guardrail_phase_is_rejected() -> None:
    from model_gateway_control.domain.tenant import GuardrailBinding

    with pytest.raises(InvalidRequestError, match="unknown guardrail phase"):
        GuardrailBinding(component="secret-scan", phases=("midflight",))


def test_policy_is_compiled_into_the_snapshot(fleet: Fleet, tenant: Tenant) -> None:
    # Compiled, never interpreted: the worker receives an ordered table and
    # scans it rather than parsing rules on every request.
    from model_gateway_control.domain.policy import PolicyBundle, PolicyEffect, PolicyRule

    guarded = dataclasses.replace(
        tenant,
        policy=PolicyBundle(
            id="acme",
            rules=(
                PolicyRule(
                    id="restrict-sensitive",
                    effect=PolicyEffect.ALLOW,
                    models=("llama-3.3-70b",),
                    data_class="restricted",
                    min_trust_tier=TrustTier.INTERNAL,
                ),
                PolicyRule(
                    id="deny-outside-corp",
                    effect=PolicyEffect.DENY,
                    source_cidrs=("10.0.0.0/8",),
                    reason="outside the corporate network",
                ),
            ),
        ),
    )

    snapshot = build_snapshot(fleet, [guarded], BUILT_AT)
    compiled = snapshot.tenants[0].policy

    assert [r.id for r in compiled.rules] == ["restrict-sensitive", "deny-outside-corp"]
    assert compiled.rules[0].data_class == "restricted"
    assert compiled.rules[1].reason == "outside the corporate network"


def test_an_unparseable_network_fails_the_build() -> None:
    # Rejected here rather than at the worker. A network rule that silently
    # does not apply is a restriction an operator believes is in force, and
    # finding out at build time costs a failed build rather than a hole.
    from model_gateway_control.domain.policy import PolicyEffect, PolicyRule

    with pytest.raises(InvalidRequestError, match="unparseable network"):
        PolicyRule(id="bad", effect=PolicyEffect.DENY, source_cidrs=("10.0.0.0/99",))


def test_duplicate_rule_ids_are_rejected() -> None:
    # Two rules with one id makes "which rule refused this" — the question a
    # denial exists to answer — unanswerable.
    from model_gateway_control.domain.policy import PolicyBundle, PolicyEffect, PolicyRule

    rule = PolicyRule(id="same", effect=PolicyEffect.ALLOW)
    with pytest.raises(InvalidRequestError, match="duplicate policy rule"):
        PolicyBundle(rules=(rule, rule))


# --- the registry gate ------------------------------------------------------
#
# The registry is only a gate if the builder refuses what it cannot vouch for.
# Every case below would otherwise compile into a snapshot and reach every
# worker, where an unresolvable binding is a warning in a log at request time.


def _guarded(tenant: Tenant, **binding: object) -> Tenant:
    from model_gateway_control.domain.tenant import GuardrailBinding

    return dataclasses.replace(
        tenant,
        plugins=(),
        guardrails=(GuardrailBinding(**binding),),  # type: ignore[arg-type]
    )


def test_a_binding_naming_an_unregistered_component_does_not_compile(
    fleet: Fleet, tenant: Tenant
) -> None:
    bare = dataclasses.replace(fleet, registry=Registry(), default_plugins=())

    with pytest.raises(InvalidRequestError, match="regex-pii"):
        build_snapshot(bare, [dataclasses.replace(tenant, plugins=fleet.default_plugins)], BUILT_AT)


def test_a_binding_naming_a_pending_component_does_not_compile(
    fleet: Fleet, tenant: Tenant
) -> None:
    # Registered is not admitted. If this compiled, "register" would be the
    # call that grants production access.
    from model_gateway_control.domain.component import Component, Manifest

    pending = Component(
        manifest=Manifest(
            name="untested", version="1.0.0", port=Port.GUARDRAIL, latency_budget_ms=50
        )
    )
    registered = dataclasses.replace(fleet, registry=Registry((pending,)), default_plugins=())

    with pytest.raises(InvalidRequestError, match="pending"):
        build_snapshot(
            registered, [_guarded(tenant, component="untested", version="1.0.0")], BUILT_AT
        )


def test_a_binding_naming_a_retired_component_does_not_compile(
    fleet: Fleet, tenant: Tenant
) -> None:
    # Retiring has to actually stop new snapshots, or it is a label.
    retired = dataclasses.replace(guardrail_component("presidio", "2.1.0"), status=Status.RETIRED)
    registered = dataclasses.replace(fleet, registry=Registry((retired,)), default_plugins=())

    with pytest.raises(InvalidRequestError, match="retired"):
        build_snapshot(
            registered, [_guarded(tenant, component="presidio", version="2.1.0")], BUILT_AT
        )


def test_a_binding_that_starves_a_guardrail_of_its_declared_budget_does_not_compile(
    fleet: Fleet, tenant: Tenant
) -> None:
    # The guardrail would be cut off on every request. Which way that fails
    # depends on the failure mode, and both ways are wrong.
    registered = dataclasses.replace(
        fleet,
        registry=Registry((guardrail_component("presidio", "2.1.0", latency_budget_ms=200),)),
        default_plugins=(),
    )

    with pytest.raises(InvalidRequestError, match="needs 200ms"):
        build_snapshot(
            registered,
            [_guarded(tenant, component="presidio", version="2.1.0", timeout_ms=50)],
            BUILT_AT,
        )


def test_a_fleet_default_binding_is_checked_too(fleet: Fleet) -> None:
    # Fleet defaults apply to every tenant that declares nothing, so an
    # unchecked one is the widest possible version of this mistake.
    bare = dataclasses.replace(fleet, registry=Registry())

    with pytest.raises(InvalidRequestError, match="fleet defaults"):
        build_snapshot(bare, [], BUILT_AT)


def test_a_control_plane_component_cannot_be_bound_into_a_snapshot(
    fleet: Fleet, tenant: Tenant
) -> None:
    # A trainer never reaches a worker. Encoding the binding anyway would ship
    # a port the data plane silently ignores.
    from model_gateway_control.domain.component import Manifest, admitted
    from model_gateway_control.domain.tenant import PluginBinding

    trainer = admitted(
        Manifest(name="llamafactory", version="0.9.0", port=Port.TRAINER),
        suite_version="1",
        runner="test",
    )
    registered = dataclasses.replace(
        fleet,
        registry=Registry((trainer,)),
        default_plugins=(PluginBinding(port=Port.TRAINER, component="llamafactory"),),
    )

    with pytest.raises(InvalidRequestError, match="do not run in the data plane"):
        build_snapshot(registered, [dataclasses.replace(tenant, plugins=())], BUILT_AT)


# --- publisher signatures ---------------------------------------------------


def _signed_component(name: str, key_id: str) -> tuple[Component, PublisherKey]:
    """An admitted guardrail whose manifest is signed by a fresh key."""
    private = Ed25519PrivateKey.generate()
    public = private.public_key().public_bytes(encoding=Encoding.Raw, format=PublicFormat.Raw)
    key = PublisherKey(key_id=key_id, publisher="ACME", public_key=public)

    manifest = Manifest(name=name, version="1.0.0", port=Port.GUARDRAIL, latency_budget_ms=50)
    component = admitted(
        manifest,
        suite_version="1",
        runner="test",
        signature=sign(manifest.digest(), private, key_id),
    )
    return component, key


def _bound(fleet: Fleet, tenant: Tenant, component: Component) -> tuple[Fleet, Tenant]:
    from model_gateway_control.domain.tenant import GuardrailBinding

    return (
        dataclasses.replace(fleet, registry=Registry((component,)), default_plugins=()),
        dataclasses.replace(
            tenant,
            plugins=(),
            guardrails=(GuardrailBinding(component=component.manifest.name, timeout_ms=50),),
        ),
    )


def test_a_snapshot_rechecks_the_signature_rather_than_trusting_the_registry(
    fleet: Fleet, tenant: Tenant
) -> None:
    # Registration verified this too, but registration's answer lives in the
    # database — and so does the status that says a component is bindable. If
    # both came from there, whoever could write one could write the other.
    component, key = _signed_component("acme-guard", "acme-2026")
    bound_fleet, bound_tenant = _bound(fleet, tenant, component)

    snapshot = build_snapshot(bound_fleet, [bound_tenant], BUILT_AT, TrustStore(keys=(key,)))

    assert snapshot.tenants[0].guardrails[0].component == "acme-guard"


def test_a_component_whose_signature_does_not_verify_cannot_be_bound(
    fleet: Fleet, tenant: Tenant
) -> None:
    # The check that actually gates production: getting a forged component into
    # a snapshot takes the trusted-keys file, not just the database.
    component, _ = _signed_component("acme-guard", "acme-2026")
    _, someone_else = _signed_component("other", "acme-2026")
    bound_fleet, bound_tenant = _bound(fleet, tenant, component)

    with pytest.raises(InvalidRequestError, match="does not match this manifest"):
        build_snapshot(bound_fleet, [bound_tenant], BUILT_AT, TrustStore(keys=(someone_else,)))


def test_revoking_a_key_takes_its_components_out_of_the_next_snapshot(
    fleet: Fleet, tenant: Tenant
) -> None:
    # A key is revoked because it is believed compromised, and its components
    # are exactly the ones that must stop running — not whenever an operator
    # remembers to retire them one by one.
    component, key = _signed_component("acme-guard", "acme-2026")
    bound_fleet, bound_tenant = _bound(fleet, tenant, component)
    revoked = dataclasses.replace(key, status=KeyStatus.REVOKED)

    with pytest.raises(InvalidRequestError, match="revoked"):
        build_snapshot(bound_fleet, [bound_tenant], BUILT_AT, TrustStore(keys=(revoked,)))


def test_a_signed_component_fails_loudly_when_no_keys_are_configured(
    fleet: Fleet, tenant: Tenant
) -> None:
    # A signature nobody can check must never pass for a valid one. Failing
    # here is a misconfiguration an operator can see; passing would be a
    # security control that silently is not one.
    component, _ = _signed_component("acme-guard", "acme-2026")
    bound_fleet, bound_tenant = _bound(fleet, tenant, component)

    with pytest.raises(InvalidRequestError, match="not a trusted publisher key"):
        build_snapshot(bound_fleet, [bound_tenant], BUILT_AT)


def test_an_unsigned_component_still_builds_when_signatures_are_optional(
    fleet: Fleet, tenant: Tenant
) -> None:
    unsigned = guardrail_component("plain-guard", "1.0.0")
    bound_fleet, bound_tenant = _bound(fleet, tenant, unsigned)

    snapshot = build_snapshot(bound_fleet, [bound_tenant], BUILT_AT, TrustStore())

    assert snapshot.tenants[0].guardrails[0].component == "plain-guard"
