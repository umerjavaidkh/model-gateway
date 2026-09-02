"""Command-line entry points for the control plane.

Until the admin API lands this is how a snapshot gets built. It stays afterwards
as the tool that produces fixtures for the data plane's tests and for the load
harness, which is why it takes a JSON description rather than reading a
database: a test needs a deterministic artifact, not a deployment.
"""

from __future__ import annotations

import argparse
import base64
import json
import sys
from collections.abc import Sequence
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import (
    Encoding,
    NoEncryption,
    PrivateFormat,
    PublicFormat,
    load_pem_private_key,
)

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
    Port,
    Registry,
    admitted,
    manifest_from_dict,
)
from model_gateway_control.domain.identity import BudgetRef, Principal, RateLimit, issue_key
from model_gateway_control.domain.policy import PolicyBundle, PolicyEffect, PolicyRule
from model_gateway_control.domain.signing import KeyStatus
from model_gateway_control.domain.signing import sign as sign_digest
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

    digest = sub.add_parser(
        "component-digest", help="validate a component manifest and print its digest"
    )
    digest.add_argument("--manifest", type=Path, required=True, help="JSON manifest")

    keygen = sub.add_parser("keygen", help="generate an Ed25519 publisher signing key")
    keygen.add_argument("--key-id", required=True, help="how the trust store will name this key")
    keygen.add_argument("--publisher", required=True, help="who holds it")
    keygen.add_argument("--out", type=Path, required=True, help="file to write the private key to")

    sign = sub.add_parser("sign-manifest", help="sign a component manifest for submission")
    sign.add_argument("--manifest", type=Path, required=True, help="JSON manifest")
    sign.add_argument("--key", type=Path, required=True, help="private key from keygen")
    sign.add_argument("--key-id", required=True, help="the id the trust store knows this key by")

    args = parser.parse_args(argv)
    try:
        if args.command == "component-digest":
            return _component_digest(args.manifest)
        if args.command == "keygen":
            return _keygen(args.key_id, args.publisher, args.out)
        if args.command == "sign-manifest":
            return _sign_manifest(args.manifest, args.key, args.key_id)
        return _build_snapshot(args.config, args.out, args.pepper)
    except GatewayError as err:
        print(f"gatewayctl: {err}", file=sys.stderr)
        return 1


def _component_digest(manifest_path: Path) -> int:
    """Validate a manifest locally and print what an admission must bind to.

    A publisher needs both before submitting: the validation so the API's
    rejection is not the first feedback they get, and the digest because a
    contract-suite runner has to say which manifest it examined and cannot
    compute that from a name and a version.
    """
    manifest = manifest_from_dict(json.loads(manifest_path.read_text()))
    print(manifest.digest())
    print(f"  {manifest.ref} fills {manifest.port}, {manifest.execution}", file=sys.stderr)
    return 0


def _keygen(key_id: str, publisher: str, out: Path) -> int:
    """Generate a signing key and print the trust-store entry for its public half.

    The private key is written with owner-only permissions and never printed:
    a key that appears in a terminal is a key in a scrollback buffer, a shell
    history file, and whatever collects the CI logs.
    """
    if out.exists():
        # Overwriting a signing key silently is how a publisher discovers that
        # everything they signed last year can no longer be attributed.
        raise InvalidRequestError(f"{out} already exists; refusing to overwrite a signing key")

    private = Ed25519PrivateKey.generate()
    out.write_bytes(
        private.private_bytes(
            encoding=Encoding.PEM,
            format=PrivateFormat.PKCS8,
            encryption_algorithm=NoEncryption(),
        )
    )
    out.chmod(0o600)

    public = private.public_key().public_bytes(encoding=Encoding.Raw, format=PublicFormat.Raw)
    entry = {
        "key_id": key_id,
        "publisher": publisher,
        "public_key": base64.b64encode(public).decode(),
        "status": str(KeyStatus.ACTIVE),
    }
    print(json.dumps({"keys": [entry]}, indent=2))
    print(
        f"  private key written to {out} (0600); add the block above to the trust store",
        file=sys.stderr,
    )
    return 0


def _sign_manifest(manifest_path: Path, key_path: Path, key_id: str) -> int:
    """Sign a manifest's digest and print what a registration request needs.

    The manifest is parsed and validated first, so a publisher signs something
    the control plane will accept rather than discovering a schema error after
    the signature is already circulating.
    """
    manifest = manifest_from_dict(json.loads(manifest_path.read_text()))

    loaded = load_pem_private_key(key_path.read_bytes(), password=None)
    if not isinstance(loaded, Ed25519PrivateKey):
        raise InvalidRequestError(f"{key_path} is not an Ed25519 private key")

    signature = sign_digest(manifest.digest(), loaded, key_id)
    print(json.dumps({"signing_key_id": key_id, "signature": signature.encoded()}, indent=2))
    print(f"  covers {manifest.ref} at digest {manifest.digest()}", file=sys.stderr)
    return 0


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
        registry=_parse_registry(raw.get("registry", [])),
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


def _parse_registry(raw: list[dict[str, Any]]) -> Registry:
    """Read the component registry a config file declares.

    A file may state that a component was admitted, and the admission binds to
    the manifest written beside it. That is not a hole in the gate: this path
    is an operator compiling a snapshot from a local file they already control,
    and it is how components compiled into the worker are registered at all —
    their suite runs in this repository's CI, not in a sandbox at request time.
    The gate governs the API, where the submitter is not the operator.
    """
    components = []
    for entry in raw:
        record = entry.get("admission")
        manifest = manifest_from_dict({k: v for k, v in entry.items() if k != "admission"})
        if record is None:
            components.append(Component(manifest=manifest))
            continue
        components.append(
            admitted(
                manifest,
                suite_version=str(record["suite_version"]),
                runner=str(record["runner"]),
                evidence_ref=str(record.get("evidence_ref", "")),
                passed=bool(record.get("passed", True)),
            )
        )
    return Registry(tuple(components))


def _parse_plugin(raw: dict[str, Any]) -> PluginBinding:
    return PluginBinding(
        port=Port(raw["port"]),
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
