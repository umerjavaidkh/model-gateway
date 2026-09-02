"""Deciding whether a canary has earned its next step.

The eval gate (ADR 0014) established that an adapter is better on a fixed
suite before any traffic reached it. This is the other half: whether it holds
up under *this tenant's* traffic, at the concurrency production actually runs
at, on the prompts the suite never contained.

# What "healthy" can honestly mean here

Operational signals only — does it error, how long does it take. Not quality.
Judging whether an answer was *better* means storing both payloads, which this
system deliberately does not do, or running a judge over them, which is what
the eval suite already is.

That matters because "we shadow traffic and promote automatically" is easily
heard as "we compare answers", and a promotion resting on that
misunderstanding would be resting on nothing. What automatic promotion buys is
protection from the adapter that passed its suite and then fell over on real
traffic — which is a real and common failure, and not the same as the adapter
that is subtly worse.

Subtly worse is the eval gate's problem, and it was answered before this
started.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timedelta

from sqlalchemy import func, select
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.domain.scorecard import BASIS_POINTS
from model_gateway_control.errors import InvalidRequestError

#: How long a step must run before it can be judged.
#:
#: A step judged the instant it starts is judged on nothing. This is a floor on
#: elapsed time; MIN_REQUESTS is the floor on evidence, and both have to clear.
DEFAULT_DWELL = timedelta(minutes=15)

#: How many requests a step needs before its error rate means anything.
#:
#: Thirty is not a statistical threshold, it is a refusal to divide by three.
#: A canary at 1% of a quiet tenant's traffic may never reach it, and that is
#: the correct outcome: it waits, and an operator advances it by hand knowing
#: they are doing so without evidence.
MIN_REQUESTS = 30

#: How much worse than the base model an adapter may be before a step is
#: refused, in basis points of error rate.
#:
#: Not zero. Real traffic has a noise floor — a provider blip during the
#: canary's window and not the base model's would abort every rollout — and a
#: gate that aborts on noise is one an operator stops using.
DEFAULT_ERROR_TOLERANCE = 200


@dataclass(frozen=True, slots=True)
class Observed:
    """What real traffic did to one deployment over a window."""

    requests: int
    errors: int

    @property
    def error_rate(self) -> int:
        """Errors per 10,000 requests. Integer, like every other rate here."""
        if self.requests == 0:
            return 0
        return self.errors * BASIS_POINTS // self.requests


@dataclass(frozen=True, slots=True)
class Verdict:
    """Whether a step may be taken, and why not."""

    #: Advance, abort, or neither. Three outcomes rather than a boolean: "not
    #: yet" and "no" are different, and collapsing them either aborts healthy
    #: rollouts that are merely quiet or advances ones nothing has measured.
    advance: bool
    abort: bool
    reason: str

    @property
    def wait(self) -> bool:
        """Whether the decision is to do nothing for now."""
        return not self.advance and not self.abort


@dataclass(frozen=True, slots=True)
class Policy:
    """When a canary step may be taken.

    A value rather than three constructor arguments, so a deployment's
    thresholds are one thing that can be passed around, logged and compared
    rather than three that can be passed in the wrong order.
    """

    dwell: timedelta = DEFAULT_DWELL
    min_requests: int = MIN_REQUESTS
    error_tolerance: int = DEFAULT_ERROR_TOLERANCE

    def __post_init__(self) -> None:
        if self.min_requests <= 0:
            raise InvalidRequestError("a rollout needs some minimum evidence")
        if self.error_tolerance < 0:
            raise InvalidRequestError("an error tolerance cannot be negative")


class RolloutHealth:
    """Reads what traffic did to an adapter and its base model."""

    def __init__(self, session: AsyncSession, policy: Policy | None = None) -> None:
        self._session = session
        self._policy = policy or Policy()

    async def observe(self, deployment: str, since: datetime) -> Observed:
        """Count what one deployment served since a moment.

        Shadow and real traffic together: an adapter's errors are its errors
        whether or not anybody was waiting for the answer, and separating them
        would mean a canary's first steps were judged on nothing while its
        shadow window was ignored.
        """
        row = (
            await self._session.execute(
                select(
                    func.count().label("requests"),
                    # A non-empty outcome is the gateway's error code; empty
                    # is success. Counted with a filtered aggregate rather than
                    # summing a cast boolean, which the two dialects disagree
                    # about.
                    func.count().filter(models.UsageRecord.outcome != "").label("errors"),
                ).where(
                    models.UsageRecord.deployment == deployment,
                    models.UsageRecord.occurred_at >= since,
                )
            )
        ).one()
        return Observed(requests=int(row.requests or 0), errors=int(row.errors or 0))

    async def judge(self, adapter: str, base: str, started_at: datetime, now: datetime) -> Verdict:
        """Decide whether the adapter has earned its next step."""
        if now - started_at < self._policy.dwell:
            return Verdict(
                advance=False,
                abort=False,
                reason=(
                    f"the step has run for {now - started_at}, "
                    f"less than the {self._policy.dwell} dwell"
                ),
            )

        measured = await self.observe(adapter, started_at)
        if measured.requests < self._policy.min_requests:
            # Waiting, not failing. A canary at 1% of a quiet tenant's traffic
            # may never reach the floor, and advancing it on three requests
            # would be worse than admitting there is no evidence.
            return Verdict(
                advance=False,
                abort=False,
                reason=(
                    f"{measured.requests} requests is not enough to judge; "
                    f"{self._policy.min_requests} are needed"
                ),
            )

        against = await self.observe(base, started_at)
        excess = measured.error_rate - against.error_rate
        if excess > self._policy.error_tolerance:
            return Verdict(
                advance=False,
                abort=True,
                reason=(
                    f"errors at {_pct(measured.error_rate)} against the base model's "
                    f"{_pct(against.error_rate)}, over the "
                    f"{_pct(self._policy.error_tolerance)} tolerance"
                ),
            )

        return Verdict(
            advance=True,
            abort=False,
            reason=(
                f"{measured.requests} requests at {_pct(measured.error_rate)} errors, "
                f"against the base model's {_pct(against.error_rate)}"
            ),
        )


def _pct(basis_points: int) -> str:
    return f"{basis_points / 100:.2f}%"
