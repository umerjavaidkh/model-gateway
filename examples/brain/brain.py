"""A compliance engine, simulated in one class.

Not a real brain. It exists to prove the seams are the right ones, by doing the
three things a compliance engine does and doing them the way the architecture
says they should be done:

  1. **Publishes rules.** It authors policy and hands it to the gateway, which
     compiles it into the next snapshot. Every worker then evaluates it locally
     in microseconds. The brain is never called during a request.

  2. **Grants access.** Same seam — approving a model is a rule, not a callback.

  3. **Answers questions.** It reads the usage stream, which the architecture
     already treats as the place consumers attach: "cost accounting, audit
     table, and SIEM forwarding are all consumers, so adding one never touches
     the gateway."

The point of the exercise is what is *absent*: no hook in the request path, no
network call per completion, and nothing that stops traffic when the brain is
down. A compliance engine that had to be consulted per request would be a hard
dependency of every completion, and the first outage would prove it.

Run it against the local fleet:

    python examples/brain/brain.py approve qwen2.5:0.5b
    python examples/brain/brain.py revoke qwen2.5:0.5b
    python examples/brain/brain.py report
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from collections import Counter
from dataclasses import dataclass
from typing import Any

DEFAULT_ADMIN = os.environ.get("GATEWAY_ADMIN_URL", "http://localhost:18081")
DEFAULT_TOKEN = os.environ.get("GATEWAY_ADMIN_TOKEN", "local-development-admin-token-32ch")
DEFAULT_REDIS = os.environ.get("GATEWAY_REDIS_URL", "redis://localhost:16379")
STREAM = "gateway:usage"


@dataclass
class Brain:
    """A compliance authority that publishes rules and reads outcomes.

    Deliberately one class and deliberately dumb: every decision here is a
    constant. A real engine would reach these conclusions from regulation,
    contracts and risk appetite — but it would reach them *the same way*, on
    its own schedule, and publish them through the same seam.
    """

    admin_url: str = DEFAULT_ADMIN
    token: str = DEFAULT_TOKEN
    redis_url: str = DEFAULT_REDIS

    # --- 1 and 2: rules and access -------------------------------------

    def approve(self, *models: str) -> dict[str, Any]:
        """Allow only these models, and refuse everything else.

        Two rules, in order, because first match wins: an allow that names the
        approved models, then a deny that catches whatever the first did not.
        Expressing "only these" as an allowlist plus a terminal deny is the
        shape a decision table gives you, and it is worth noticing that it
        reads exactly like a compliance statement.
        """
        return self._publish(
            [
                {
                    "id": "approved-models",
                    "effect": "allow",
                    "models": list(models),
                },
                {
                    "id": "unapproved-model",
                    "effect": "deny",
                    # Returned to the caller, so it must be safe to disclose
                    # and useful to whoever reads it.
                    "reason": "compliance: this model is not approved for use",
                },
            ]
        )

    def revoke(self, *models: str) -> dict[str, Any]:
        """Withdraw approval, denying these models by name.

        The deny comes first, so it wins over anything permissive after it.
        This is the case worth watching: the rule reaches every worker within
        one snapshot poll, and no request was ever routed through the brain to
        make it happen.
        """
        return self._publish(
            [
                {
                    "id": "revoked-models",
                    "effect": "deny",
                    "models": list(models),
                    "reason": "compliance: approval for this model has been withdrawn",
                },
                {"id": "everything-else", "effect": "allow"},
            ]
        )

    def open_access(self) -> dict[str, Any]:
        """Withdraw every rule. Policy is one control among several."""
        return self._publish([])

    def current(self) -> dict[str, Any]:
        """What the gateway believes the policy is."""
        return self._call("GET", "/v1/policy")

    # --- 3: answering questions ----------------------------------------

    def report(self, limit: int = 500) -> str:
        """Summarise what actually happened, from the usage stream.

        Reading rather than being asked. The gateway does not know this
        consumer exists, which is the property that lets a compliance engine be
        added to a running system without touching it.
        """
        try:
            import redis
        except ImportError:  # pragma: no cover - a convenience, not a feature
            return "install redis to read the stream: pip install redis"

        client = redis.Redis.from_url(self.redis_url)
        entries = client.xrevrange(STREAM, count=limit)

        outcomes: Counter[str] = Counter()
        models: Counter[str] = Counter()
        cost = 0
        for _, fields in entries:
            event = self._decode(fields)
            if event is None:
                continue
            outcomes[event.get("outcome") or "ok"] += 1
            models[event.get("base_model") or "?"] += 1
            cost += int(event.get("cost_micro_usd") or 0)

        served = sum(outcomes.values())
        lines = [
            f"{served} requests seen",
            f"  spend      {cost / 1_000_000:.6f} USD",
            f"  models     {dict(models) or 'none'}",
            f"  outcomes   {dict(outcomes) or 'none'}",
        ]
        refused = outcomes.get("forbidden", 0)
        if refused:
            lines.append(f"  {refused} refused by policy — the rules are being enforced")
        return "\n".join(lines)

    @staticmethod
    def _decode(fields: dict[bytes, bytes]) -> dict[str, Any] | None:
        """Read a usage event off the stream.

        The payload is protobuf, so this reaches for the generated type when it
        is importable and gives up quietly when it is not — a simulator that
        cannot read the stream should say so rather than crash.
        """
        payload = fields.get(b"event")
        if payload is None:
            return None
        try:
            from google.protobuf.json_format import MessageToDict

            from model_gateway_control.wire import usage_pb2

            event = usage_pb2.UsageEvent()
            event.ParseFromString(payload)
            return MessageToDict(event, preserving_proto_field_name=True)
        except Exception:
            return None

    # --- the seam ------------------------------------------------------

    def _publish(self, rules: list[dict[str, Any]]) -> dict[str, Any]:
        """Replace the fleet policy with this rule set.

        Whole, not a patch. The brain restates its current position and is
        done; it never has to remember what it said last time, and a retry
        after a crash is free.
        """
        return self._call("PUT", "/v1/policy", {"rules": rules})

    def _call(self, method: str, path: str, body: dict[str, Any] | None = None) -> dict[str, Any]:
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(
            f"{self.admin_url}{path}",
            data=data,
            method=method,
            headers={
                "Authorization": f"Bearer {self.token}",
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                return json.loads(response.read() or "{}")
        except urllib.error.HTTPError as err:
            detail = err.read().decode(errors="replace")
            raise SystemExit(f"the gateway refused: {err.code} {detail}") from err
        except urllib.error.URLError as err:
            raise SystemExit(f"cannot reach the gateway at {self.admin_url}: {err}") from err


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="brain", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    approve = sub.add_parser("approve", help="allow only these models")
    approve.add_argument("models", nargs="+")

    revoke = sub.add_parser("revoke", help="withdraw approval for these models")
    revoke.add_argument("models", nargs="+")

    sub.add_parser("open", help="withdraw every rule")
    sub.add_parser("policy", help="show what the gateway believes")
    sub.add_parser("report", help="summarise what traffic actually did")

    args = parser.parse_args(argv)
    brain = Brain()

    if args.command == "approve":
        print(json.dumps(brain.approve(*args.models), indent=2))
    elif args.command == "revoke":
        print(json.dumps(brain.revoke(*args.models), indent=2))
    elif args.command == "open":
        print(json.dumps(brain.open_access(), indent=2))
    elif args.command == "policy":
        print(json.dumps(brain.current(), indent=2))
    else:
        print(brain.report())
    return 0


if __name__ == "__main__":
    sys.exit(main())
