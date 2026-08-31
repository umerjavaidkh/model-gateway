from __future__ import annotations

from model_gateway_control.errors import Code, ConflictError, GatewayError, NotFoundError


def test_every_error_carries_its_code() -> None:
    assert NotFoundError("no such tenant").code is Code.NOT_FOUND
    assert ConflictError("stale version").code is Code.CONFLICT


def test_str_includes_the_code_so_logs_are_greppable() -> None:
    assert str(NotFoundError("no such tenant")) == "not_found: no such tenant"


def test_cause_is_preserved_for_traceback_chaining() -> None:
    cause = ValueError("bad row")
    err = GatewayError("compiling the snapshot failed", cause=cause)
    assert err.__cause__ is cause
