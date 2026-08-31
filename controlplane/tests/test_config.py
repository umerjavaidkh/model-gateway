from __future__ import annotations

import pytest

from model_gateway_control.config import DEFAULT_ADMIN_PORT, Settings
from model_gateway_control.errors import InvalidRequestError


def test_defaults_apply_when_optional_variables_are_absent() -> None:
    settings = Settings.from_env({"GATEWAY_DATABASE_URL": "postgres://localhost/gw"})

    assert settings.admin_port == DEFAULT_ADMIN_PORT
    assert settings.database_url == "postgres://localhost/gw"


def test_missing_database_url_is_rejected_at_construction() -> None:
    with pytest.raises(InvalidRequestError, match="database_url"):
        Settings.from_env({})


@pytest.mark.parametrize("port", ["0", "70000"])
def test_out_of_range_port_is_rejected(port: str) -> None:
    env = {"GATEWAY_DATABASE_URL": "postgres://localhost/gw", "GATEWAY_ADMIN_PORT": port}
    with pytest.raises(InvalidRequestError, match="admin_port"):
        Settings.from_env(env)


def test_unparseable_integer_names_the_variable() -> None:
    env = {"GATEWAY_DATABASE_URL": "postgres://localhost/gw", "GATEWAY_ADMIN_PORT": "eighty"}
    with pytest.raises(InvalidRequestError, match="GATEWAY_ADMIN_PORT"):
        Settings.from_env(env)


def test_settings_are_immutable() -> None:
    settings = Settings(database_url="postgres://localhost/gw")
    with pytest.raises(AttributeError):
        settings.admin_port = 9000  # type: ignore[misc]
