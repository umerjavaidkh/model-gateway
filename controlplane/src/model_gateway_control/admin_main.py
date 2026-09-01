"""Run the admin API.

A separate process from the data plane, on its own listener. The plan puts mTLS
in front of it; that is deployment configuration, and the bearer token checked
in the application is the in-process half rather than a replacement for it.
"""

from __future__ import annotations

import os

import uvicorn

from model_gateway_control.api.app import AdminSettings, create_app
from model_gateway_control.db.session import create_engine
from model_gateway_control.errors import InvalidRequestError

DEFAULT_PORT = 8081
MIN_TOKEN_LENGTH = 32


def main() -> int:
    """Start the admin API. Returns a process exit code."""
    database_url = os.environ.get("GATEWAY_DATABASE_URL", "")
    pepper = os.environ.get("GATEWAY_KEY_PEPPER", "").encode()
    token = os.environ.get("GATEWAY_ADMIN_TOKEN", "")

    if not database_url:
        raise InvalidRequestError("GATEWAY_DATABASE_URL is required")
    if len(pepper) < MIN_TOKEN_LENGTH:
        raise InvalidRequestError(
            f"GATEWAY_KEY_PEPPER must be at least {MIN_TOKEN_LENGTH} bytes "
            "and must match the data plane"
        )
    if len(token) < MIN_TOKEN_LENGTH:
        # A short admin token is worse than a missing one, because it looks
        # configured. This surface can issue credentials for every provider the
        # organisation uses.
        raise InvalidRequestError(
            f"GATEWAY_ADMIN_TOKEN must be at least {MIN_TOKEN_LENGTH} characters"
        )

    app = create_app(
        AdminSettings(engine=create_engine(database_url), key_pepper=pepper, admin_token=token)
    )
    uvicorn.run(
        app,
        host=os.environ.get("GATEWAY_ADMIN_HOST", "127.0.0.1"),
        port=int(os.environ.get("GATEWAY_ADMIN_PORT", DEFAULT_PORT)),
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
