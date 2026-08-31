"""Process configuration.

Settings are read from a mapping that is passed in, not from ``os.environ``
directly. That one choice is what makes configuration testable without patching
module globals — the tell that a dependency should have been an argument.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass

from model_gateway_control.errors import InvalidRequestError

DEFAULT_ADMIN_PORT = 8081
DEFAULT_SNAPSHOT_INTERVAL_SECONDS = 15


@dataclass(frozen=True, slots=True, kw_only=True)
class Settings:
    """Immutable process configuration.

    Frozen because configuration that changes under a running process is a
    category of bug nobody enjoys; keyword-only because a positional constructor
    with this many same-typed fields is fragile to reordering.
    """

    database_url: str
    admin_port: int = DEFAULT_ADMIN_PORT
    snapshot_interval_seconds: int = DEFAULT_SNAPSHOT_INTERVAL_SECONDS

    def __post_init__(self) -> None:
        # Validate at construction, so no caller has to re-check at use.
        if not self.database_url:
            raise InvalidRequestError("database_url must not be empty")
        if not 1 <= self.admin_port <= 65535:
            raise InvalidRequestError(f"admin_port {self.admin_port} is out of range")
        if self.snapshot_interval_seconds < 1:
            raise InvalidRequestError("snapshot_interval_seconds must be at least 1")

    @classmethod
    def from_env(cls, env: Mapping[str, str]) -> Settings:
        """Build settings from an environment mapping.

        Raises:
            InvalidRequestError: if a required variable is missing or unparseable.
        """
        database_url = env.get("GATEWAY_DATABASE_URL", "")
        return cls(
            database_url=database_url,
            admin_port=_read_int(env, "GATEWAY_ADMIN_PORT", DEFAULT_ADMIN_PORT),
            snapshot_interval_seconds=_read_int(
                env, "GATEWAY_SNAPSHOT_INTERVAL_SECONDS", DEFAULT_SNAPSHOT_INTERVAL_SECONDS
            ),
        )


def _read_int(env: Mapping[str, str], name: str, default: int) -> int:
    raw = env.get(name)
    if raw is None or raw == "":
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise InvalidRequestError(f"{name} must be an integer, got {raw!r}") from exc
