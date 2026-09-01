"""Spend limits."""

from __future__ import annotations

from dataclasses import dataclass
from enum import IntEnum

from model_gateway_control.errors import InvalidRequestError

#: Fraction of a hard limit held back, in basis points, so that requests
#: already streaming when a budget tips can finish without overshooting.
DEFAULT_HEADROOM_BASIS_POINTS = 500


class BudgetScope(IntEnum):
    """Which level of the identity hierarchy a budget attaches to.

    ``TRAINING`` is its own scope rather than a line item under inference: a
    single fine-tuning run can cost more than a month of serving, so it gets its
    own limit and its own approval threshold.
    """

    UNSET = 0
    KEY = 1
    APP = 2
    USER = 3
    TEAM = 4
    ORG = 5
    MODEL = 6
    TRAINING = 7


@dataclass(frozen=True, slots=True, kw_only=True)
class Budget:
    """A limit and what has been spent against it.

    Budgets are eventually consistent by design: usage events flow to an
    accounting consumer, which folds the result into the next snapshot. Rate
    limits are the mechanism for anything that must be immediate.
    """

    id: str
    scope: BudgetScope
    limit_micro_usd: int
    spent_micro_usd: int = 0
    #: Hard budgets deny on exhaustion; soft ones warn and let the request pass.
    hard: bool = True
    headroom_basis_points: int = DEFAULT_HEADROOM_BASIS_POINTS

    def __post_init__(self) -> None:
        if not self.id:
            raise InvalidRequestError("a budget needs an id")
        if self.scope is BudgetScope.UNSET:
            raise InvalidRequestError(f"budget {self.id!r} has an unset scope")
        if self.limit_micro_usd < 0 or self.spent_micro_usd < 0:
            raise InvalidRequestError(f"budget {self.id!r} has a negative amount")
        if not 0 <= self.headroom_basis_points <= 10_000:
            raise InvalidRequestError(f"budget {self.id!r} headroom is out of range")

    @property
    def available_micro_usd(self) -> int:
        """Spend remaining before the headroom band begins."""
        reserved = self.limit_micro_usd * self.headroom_basis_points // 10_000
        return max(0, self.limit_micro_usd - reserved - self.spent_micro_usd)
