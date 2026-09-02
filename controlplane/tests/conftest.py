"""Shared fixtures.

Dependencies are passed in rather than reached for, so nothing here patches a
module global — the tell that something should have been an argument.
"""

from __future__ import annotations

from datetime import UTC, datetime

import pytest

from model_gateway_control.domain.budget import Budget, BudgetScope
from model_gateway_control.domain.catalog import (
    Capability,
    Cost,
    Deployment,
    ModelAlias,
    RoutingKey,
    TrustTier,
)
from model_gateway_control.domain.component import (
    Component,
    Execution,
    Manifest,
    Port,
    Registry,
    admitted,
)
from model_gateway_control.domain.identity import (
    BudgetRef,
    Principal,
    RateLimit,
    compute_key_lookup,
)
from model_gateway_control.domain.tenant import Fleet, PluginBinding, Tenant

PEPPER = b"a-test-pepper-that-is-long-enough-32"
BUILT_AT = datetime(2026, 9, 1, 12, 0, 0, tzinfo=UTC)

ROUTE_LLAMA = RoutingKey(base_model="llama-3.3-70b")
ROUTE_ADAPTER = RoutingKey(base_model="llama-3.3-70b", adapter_id="triage-v3")
ROUTE_GPT = RoutingKey(base_model="gpt-4o-mini")


def guardrail_component(
    name: str,
    version: str,
    latency_budget_ms: int = 50,
    execution: Execution = Execution.SIDECAR,
    module: str = "",
) -> Component:
    """An admitted guardrail component, for fixtures that only need a binding to resolve."""
    return admitted(
        Manifest(
            name=name,
            version=version,
            port=Port.GUARDRAIL,
            latency_budget_ms=latency_budget_ms,
            execution=execution,
            module=module,
        ),
        suite_version="1",
        runner="test",
    )


@pytest.fixture
def fleet() -> Fleet:
    return Fleet(
        version=7,
        deployments=(
            Deployment(
                id="vllm-1",
                key=ROUTE_LLAMA,
                provider="openai-compatible",
                endpoint="http://vllm.internal:8000/v1",
                region="me-central-1",
                trust_tier=TrustTier.INTERNAL,
                weight=100,
                cost=Cost(input_per_1k_micro_usd=120, output_per_1k_micro_usd=360),
                capabilities=(Capability.STREAMING, Capability.TOOL_CALLING),
            ),
            # Weight 0: registered, taking shadow traffic, not yet serving.
            Deployment(
                id="vllm-1-triage",
                key=ROUTE_ADAPTER,
                provider="openai-compatible",
                endpoint="http://vllm.internal:8000/v1",
                trust_tier=TrustTier.INTERNAL,
                weight=0,
            ),
            Deployment(
                id="openai-1",
                key=ROUTE_GPT,
                provider="openai-compatible",
                endpoint="https://api.openai.com/v1",
                trust_tier=TrustTier.EXTERNAL,
                credential_ref="env:OPENAI_API_KEY",
                weight=100,
                cost=Cost(input_per_1k_micro_usd=150, output_per_1k_micro_usd=600),
            ),
        ),
        aliases=(ModelAlias(name="fast", targets=(ROUTE_GPT,)),),
        registry=Registry(
            (
                guardrail_component("regex-pii", "1.0.0"),
                guardrail_component("presidio", "2.1.0"),
            )
        ),
        default_plugins=(
            PluginBinding(port=Port.GUARDRAIL, component="regex-pii", version="1.0.0"),
        ),
        policy_bundle_ref="bundle-7",
    )


@pytest.fixture
def tenant() -> Tenant:
    lookup = compute_key_lookup(PEPPER, "secret-1")
    return Tenant(
        id="acme",
        tier="enterprise",
        version=3,
        key_prefixes=("acme",),
        principals=(
            Principal(
                key_id="key-1",
                tenant="acme",
                org="acme-org",
                team="platform",
                app="app-1",
                roles=("admin",),
                models_allow_all=True,
                budgets=(BudgetRef(id="monthly", scope=BudgetScope.ORG),),
                default_data_class="confidential",
                min_trust_tier=TrustTier.EXTERNAL,
                limits=RateLimit(
                    requests_per_minute=600, tokens_per_minute=90_000, max_concurrent=32
                ),
            ),
        ),
        keys={lookup: "key-1"},
        alias_overrides=(ModelAlias(name="fast", targets=(ROUTE_LLAMA,)),),
        budgets=(
            Budget(
                id="monthly",
                scope=BudgetScope.ORG,
                limit_micro_usd=5_000_000,
                spent_micro_usd=1_250_000,
            ),
        ),
        plugins=(PluginBinding(port=Port.GUARDRAIL, component="presidio", version="2.1.0"),),
        min_trust_tier=TrustTier.EXTERNAL,
    )
