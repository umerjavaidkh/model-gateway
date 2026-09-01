"""Command-line entry points for the control plane.

Until the admin API lands this is how a snapshot gets built. It stays afterwards
as the tool that produces fixtures for the data plane's tests and for the load
harness, which is why it takes a JSON description rather than reading a
database: a test needs a deterministic artifact, not a deployment.
"""

from __future__ import annotations

import argparse
import json
import sys
from collections.abc import Sequence
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from model_gateway_control.domain.budget import Budget, BudgetScope
from model_gateway_control.domain.catalog import (
    Capability,
    Cost,
    Deployment,
    ModelAlias,
    RoutingKey,
    TrustTier,
)
from model_gateway_control.domain.identity import BudgetRef, Principal, RateLimit, issue_key
from model_gateway_control.domain.policy import PolicyBundle, PolicyEffect, PolicyRule
from model_gateway_control.domain.tenant import (
    FailureMode,
    Fleet,
    GuardrailBinding,
    PluginBinding,
    Tenant,
)
from model_gateway_control.errors import GatewayError, InvalidRequestError
from model_gateway_control.snapshot import build_snapshot


def main(argv: Sequence[str] | None = None) -> int:
    """Run the CLI. Returns a process exit code rather than calling exit()."""
    parser = argparse.ArgumentParser(prog="gatewayctl", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    build = sub.add_parser("build-snapshot", help="compile a JSON description into a snapshot")
    build.add_argument("--config", type=Path, required=True, help="JSON description")
    build.add_argument("--out", type=Path, required=True, help="file to write")
    build.add_argument("--pepper", required=True, help="key pepper; must match the data plane")

    args = parser.parse_args(argv)
    try:
        return _build_snapshot(args.config, args.out, args.pepper)
    except GatewayError as err:
        print(f"gatewayctl: {err}", file=sys.stderr)
        return 1


def _build_snapshot(config_path: Path, out: Path, pepper: str) -> int:
    raw = json.loads(config_path.read_text())
    fleet = _parse_fleet(raw.get("fleet", {}))

    tenants: list[Tenant] = []
    issued: list[tuple[str, str]] = []
    for entry in raw.get("tenants", []):
        tenant, keys = _parse_tenant(entry, pepper.encode())
        tenants.append(tenant)
        issued.extend(keys)

    snapshot = build_snapshot(fleet, tenants, datetime.now(UTC))
    out.write_bytes(snapshot.SerializeToString(deterministic=True))

    print(f"wrote {out} ({out.stat().st_size} bytes)")
    for key_id, presented in issued:
        # Printed once, here, and never stored. What the control plane keeps is
        # the HMAC lookup, which is useless without the pepper.
        print(f"  {key_id}: {presented}")
    return 0


def _parse_fleet(raw: dict[str, Any]) -> Fleet:
    return Fleet(
        version=int(raw.get("version", 1)),
        deployments=tuple(_parse_deployment(d) for d in raw.get("deployments", [])),
        aliases=tuple(
            ModelAlias(name=a["name"], targets=tuple(_parse_route(t) for t in a["targets"]))
            for a in raw.get("aliases", [])
        ),
        default_plugins=tuple(_parse_plugin(p) for p in raw.get("default_plugins", [])),
        default_guardrails=tuple(_parse_guardrail(g) for g in raw.get("default_guardrails", [])),
        default_policy=_parse_policy(raw.get("default_policy")),
        policy_bundle_ref=raw.get("policy_bundle_ref", ""),
    )


def _parse_deployment(raw: dict[str, Any]) -> Deployment:
    return Deployment(
        id=raw["id"],
        key=_parse_route(raw["key"]),
        provider=raw["provider"],
        endpoint=raw["endpoint"],
        region=raw.get("region", ""),
        trust_tier=_parse_trust_tier(raw["trust_tier"]),
        credential_ref=raw.get("credential_ref", ""),
        weight=int(raw.get("weight", 0)),
        cost=Cost(
            input_per_1k_micro_usd=int(raw.get("input_cost_micro_usd", 0)),
            output_per_1k_micro_usd=int(raw.get("output_cost_micro_usd", 0)),
            cached_input_per_1k_micro_usd=int(raw.get("cached_input_cost_micro_usd", 0)),
            cache_write_per_1k_micro_usd=int(raw.get("cache_write_cost_micro_usd", 0)),
        ),
        capabilities=tuple(Capability(c) for c in raw.get("capabilities", [])),
    )


def _parse_route(raw: dict[str, Any] | str) -> RoutingKey:
    if isinstance(raw, str):
        return RoutingKey(base_model=raw)
    return RoutingKey(base_model=raw["base_model"], adapter_id=raw.get("adapter_id", ""))


def _parse_trust_tier(name: str) -> TrustTier:
    try:
        return TrustTier[name.upper()]
    except KeyError:
        raise InvalidRequestError(f"unknown trust tier {name!r}") from None


def _parse_plugin(raw: dict[str, Any]) -> PluginBinding:
    return PluginBinding(
        port=raw["port"],
        component=raw["component"],
        version=raw.get("version", ""),
        config_ref=raw.get("config_ref", ""),
    )


def _parse_policy(raw: dict[str, Any] | None) -> PolicyBundle | None:
    """Build a bundle, or None when none is declared.

    None rather than an empty bundle: "no policy of my own" falls back to the
    fleet default, and "an empty policy" deliberately does not.
    """
    if raw is None:
        return None
    return PolicyBundle(
        id=raw.get("id", ""),
        version=int(raw.get("version", 1)),
        default_effect=PolicyEffect(raw.get("default_effect", "allow")),
        rules=tuple(_parse_policy_rule(r) for r in raw.get("rules", [])),
    )


def _parse_policy_rule(raw: dict[str, Any]) -> PolicyRule:
    return PolicyRule(
        id=raw["id"],
        effect=PolicyEffect(raw["effect"]),
        models=tuple(raw.get("models", [])),
        endpoints=tuple(raw.get("endpoints", [])),
        roles=tuple(raw.get("roles", [])),
        regions=tuple(raw.get("regions", [])),
        source_cidrs=tuple(raw.get("source_cidrs", [])),
        max_payload_bytes=int(raw.get("max_payload_bytes", 0)),
        data_class=raw.get("data_class", ""),
        min_trust_tier=_parse_trust_tier(raw.get("min_trust_tier", "UNSET")),
        reason=raw.get("reason", ""),
    )


def _parse_guardrail(raw: dict[str, Any]) -> GuardrailBinding:
    return GuardrailBinding(
        component=raw["component"],
        version=raw.get("version", ""),
        config_ref=raw.get("config_ref", ""),
        timeout_ms=int(raw.get("timeout_ms", 50)),
        failure_mode=FailureMode(raw.get("failure_mode", "closed")),
        blocking=bool(raw.get("blocking", True)),
        phases=tuple(raw.get("phases", [])),
    )


def _parse_tenant(raw: dict[str, Any], pepper: bytes) -> tuple[Tenant, list[tuple[str, str]]]:
    tenant_id = raw["id"]
    prefixes = tuple(raw.get("key_prefixes", [tenant_id]))

    budgets = tuple(
        Budget(
            id=b["id"],
            scope=BudgetScope[b.get("scope", "ORG").upper()],
            limit_micro_usd=int(b["limit_micro_usd"]),
            spent_micro_usd=int(b.get("spent_micro_usd", 0)),
            hard=bool(b.get("hard", True)),
        )
        for b in raw.get("budgets", [])
    )

    principals: list[Principal] = []
    keys: dict[bytes, str] = {}
    issued: list[tuple[str, str]] = []
    for entry in raw.get("keys", []):
        key_id = entry["key_id"]
        key = issue_key(pepper, prefixes[0], key_id)
        keys[key.lookup] = key_id
        issued.append((key_id, key.presented))
        principals.append(
            Principal(
                key_id=key_id,
                tenant=tenant_id,
                org=entry.get("org", ""),
                team=entry.get("team", ""),
                user=entry.get("user", ""),
                app=entry.get("app", ""),
                roles=tuple(entry.get("roles", [])),
                models_allow_all=bool(entry.get("models_allow_all", False)),
                models=tuple(entry.get("models", [])),
                budgets=tuple(
                    BudgetRef(id=b["id"], scope=BudgetScope[b.get("scope", "ORG").upper()])
                    for b in entry.get("budgets", [])
                ),
                default_data_class=entry.get("data_class", ""),
                min_trust_tier=_parse_trust_tier(entry.get("min_trust_tier", "EXTERNAL")),
                limits=RateLimit(
                    requests_per_minute=int(entry.get("requests_per_minute", 0)),
                    tokens_per_minute=int(entry.get("tokens_per_minute", 0)),
                    max_concurrent=int(entry.get("max_concurrent", 0)),
                ),
            )
        )

    tenant = Tenant(
        id=tenant_id,
        tier=raw.get("tier", "unknown"),
        version=int(raw.get("version", 1)),
        key_prefixes=prefixes,
        principals=tuple(principals),
        keys=keys,
        alias_overrides=tuple(
            ModelAlias(name=a["name"], targets=tuple(_parse_route(t) for t in a["targets"]))
            for a in raw.get("alias_overrides", [])
        ),
        budgets=budgets,
        plugins=tuple(_parse_plugin(p) for p in raw.get("plugins", [])),
        guardrails=tuple(_parse_guardrail(g) for g in raw.get("guardrails", [])),
        policy=_parse_policy(raw.get("policy")),
        min_trust_tier=_parse_trust_tier(raw.get("min_trust_tier", "EXTERNAL")),
    )
    return tenant, issued


if __name__ == "__main__":
    raise SystemExit(main())
