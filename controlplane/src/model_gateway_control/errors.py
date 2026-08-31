"""The control plane's error vocabulary.

`Code` mirrors the Go data plane's taxonomy in `internal/core/errors.go`. The two
are kept in sync by hand today, which is a real duplication and the reason M1
introduces a single schema both sides are generated from. Until then, a change
here needs the matching change there.
"""

from __future__ import annotations

from enum import StrEnum


class Code(StrEnum):
    """Stable, machine-readable classification of a failure.

    Codes appear in audit records and in metrics, so they are part of the
    public contract: adding one is cheap, redefining one is a breaking change.
    """

    UNAUTHENTICATED = "unauthenticated"
    FORBIDDEN = "forbidden"
    INVALID_REQUEST = "invalid_request"
    NOT_FOUND = "not_found"
    CONFLICT = "conflict"
    UNAVAILABLE = "unavailable"
    INTERNAL = "internal"


class GatewayError(Exception):
    """Base class for every error the control plane raises deliberately.

    Errors from libraries are wrapped into one of these at the boundary of the
    adapter that produced them, so no caller has to know a driver's exception
    types. The HTTP mapping lives in the API layer, not here.
    """

    code: Code = Code.INTERNAL

    def __init__(self, message: str, *, cause: Exception | None = None) -> None:
        super().__init__(message)
        self.message = message
        self.__cause__ = cause

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"


class InvalidRequestError(GatewayError):
    """The caller sent something malformed or internally inconsistent."""

    code = Code.INVALID_REQUEST


class NotFoundError(GatewayError):
    """The addressed resource does not exist."""

    code = Code.NOT_FOUND


class ConflictError(GatewayError):
    """The mutation conflicts with the current state, such as a stale version."""

    code = Code.CONFLICT


class ForbiddenError(GatewayError):
    """The caller is authenticated but not permitted to do this."""

    code = Code.FORBIDDEN


class UnavailableError(GatewayError):
    """A dependency the control plane needs is down."""

    code = Code.UNAVAILABLE
