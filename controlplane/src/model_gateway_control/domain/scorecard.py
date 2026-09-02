"""Scorecards and the gate that decides whether an artifact may serve.

The eval gate is what makes agent-driven promotion safe: an adapter cannot
enter the routing pool until its suite clears a machine-checkable bar, so an
agent can promote without a human reading the numbers. That only holds if the
bar means exactly one thing, which is what this module is for.

# Two things the shape has to get right

**Which direction is worse.** A gate that says "must not regress on
latency_p95 and refusal_rate" is meaningless without knowing that lower is
better for both, while higher is better for the headline score. Getting it
backwards does not fail loudly — it passes exactly the regressions the gate
exists to catch. So a metric carries its own direction and the gate never
guesses.

**Integers, not floats.** A gate of 0.87 against a score of 0.8699999999 is a
coin toss decided by the last bits of a float, and the same comparison can go
differently on two machines. Scores are basis points: whole numbers out of
10,000, compared exactly.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum

from model_gateway_control.errors import InvalidRequestError

#: A score of 1.0 — a perfect result — in basis points.
BASIS_POINTS = 10_000


class Direction(StrEnum):
    """Which way a metric moving is an improvement."""

    #: Accuracy, pass rate, score.
    HIGHER_IS_BETTER = "higher_is_better"
    #: Latency, refusal rate, cost.
    LOWER_IS_BETTER = "lower_is_better"


@dataclass(frozen=True, slots=True, kw_only=True)
class Metric:
    """One measurement from an eval run.

    The value is an integer in whatever unit the metric is naturally counted
    in — basis points for a rate, milliseconds for a latency, micro-USD for a
    cost. The unit is recorded for display; nothing compares across units,
    because a comparison only ever happens between the same metric on two
    scorecards from the same suite.
    """

    name: str
    value: int
    direction: Direction
    unit: str = ""

    def __post_init__(self) -> None:
        if not self.name:
            raise InvalidRequestError("a metric needs a name")

    def is_worse_than(self, other: Metric) -> bool:
        """Whether this measurement is a regression against another.

        Refuses to compare metrics that disagree about their own direction:
        that means two suites, or two versions of one suite, describe the same
        name differently, and answering anyway would produce a verdict whose
        meaning depends on which side was asked.
        """
        if self.name != other.name:
            raise InvalidRequestError(f"cannot compare metric {self.name!r} against {other.name!r}")
        if self.direction is not other.direction:
            raise InvalidRequestError(
                f"metric {self.name!r} is {self.direction} on one scorecard and "
                f"{other.direction} on the other; one of them is wrong"
            )
        if self.direction is Direction.HIGHER_IS_BETTER:
            return self.value < other.value
        return self.value > other.value


@dataclass(frozen=True, slots=True, kw_only=True)
class Scorecard:
    """What an eval suite measured about one target."""

    #: The headline result, in basis points. What ``min_score`` is checked
    #: against.
    score: int
    #: Everything else the suite measured, by name.
    metrics: tuple[Metric, ...] = ()
    #: Which suite produced this, and which version of it. A scorecard compared
    #: against one from a different suite version is comparing two different
    #: measurements, so the gate refuses to.
    suite: str = ""
    suite_version: str = ""

    def __post_init__(self) -> None:
        if not 0 <= self.score <= BASIS_POINTS:
            raise InvalidRequestError(
                f"score {self.score} is not a basis-point value between 0 and {BASIS_POINTS}"
            )
        seen = set()
        for metric in self.metrics:
            if metric.name in seen:
                raise InvalidRequestError(f"scorecard reports metric {metric.name!r} twice")
            seen.add(metric.name)

    def metric(self, name: str) -> Metric | None:
        """One measurement by name."""
        for metric in self.metrics:
            if metric.name == name:
                return metric
        return None


@dataclass(frozen=True, slots=True, kw_only=True)
class PromotionGate:
    """The bar an artifact must clear before it may serve traffic.

    Recorded on a job's spec, so the bar a job faces is fixed when it is
    submitted rather than being whatever the configuration happens to say when
    it finishes. Lowering the bar cannot retroactively promote something that
    already failed it.
    """

    #: The minimum headline score, in basis points. Zero means unset, which
    #: with no must_not_regress entries is a gate that passes everything —
    #: legitimate for a job whose promotion is a human decision, and refused
    #: as a default anywhere it would be mistaken for enforcement.
    min_score: int = 0
    #: Metrics that must not be worse than the baseline's. Names only; the
    #: direction comes from the scorecard, because the gate author should not
    #: have to restate it and would eventually restate it wrongly.
    must_not_regress: tuple[str, ...] = ()

    def __post_init__(self) -> None:
        if not 0 <= self.min_score <= BASIS_POINTS:
            raise InvalidRequestError(
                f"min_score {self.min_score} is not a basis-point value "
                f"between 0 and {BASIS_POINTS}"
            )
        if len(set(self.must_not_regress)) != len(self.must_not_regress):
            raise InvalidRequestError("must_not_regress names the same metric twice")

    @property
    def needs_baseline(self) -> bool:
        """Whether deciding this gate requires evaluating the base model too.

        Only when something must not regress. A gate that is purely a minimum
        score is decided from the candidate alone, and running a second
        evaluation for it would double the cost of the gate to learn nothing.
        """
        return bool(self.must_not_regress)

    def decide(self, candidate: Scorecard, baseline: Scorecard | None = None) -> Decision:
        """Whether this artifact may serve.

        Every reason it failed, not the first: a publisher looking at a
        rejected adapter wants to know everything that has to change, rather
        than one thing per training run.
        """
        reasons = []

        if candidate.score < self.min_score:
            reasons.append(
                f"score {_pct(candidate.score)} is below the required {_pct(self.min_score)}"
            )

        if self.must_not_regress:
            reasons.extend(self._regressions(candidate, baseline))

        return Decision(passed=not reasons, reasons=tuple(reasons))

    def _regressions(self, candidate: Scorecard, baseline: Scorecard | None) -> list[str]:
        if baseline is None:
            # Not "no regressions found". A gate that silently passes when the
            # thing it compares against is missing is a gate that stops working
            # the moment a baseline run fails.
            return [
                "no baseline scorecard, so "
                f"{', '.join(self.must_not_regress)} could not be checked for regression"
            ]
        if (candidate.suite, candidate.suite_version) != (baseline.suite, baseline.suite_version):
            return [
                f"baseline was measured by {baseline.suite}@{baseline.suite_version} and the "
                f"candidate by {candidate.suite}@{candidate.suite_version}, which are not "
                "the same measurement"
            ]

        reasons = []
        for name in self.must_not_regress:
            measured, before = candidate.metric(name), baseline.metric(name)
            if measured is None or before is None:
                reasons.append(f"metric {name!r} is missing from the candidate or the baseline")
                continue
            if measured.is_worse_than(before):
                reasons.append(
                    f"{name} regressed from {before.value} to {measured.value}"
                    f"{' ' + measured.unit if measured.unit else ''}"
                )
        return reasons


@dataclass(frozen=True, slots=True, kw_only=True)
class Decision:
    """What a gate decided, and why."""

    passed: bool
    reasons: tuple[str, ...] = field(default_factory=tuple)

    def __post_init__(self) -> None:
        if self.passed and self.reasons:
            raise InvalidRequestError("a passing decision cannot carry reasons for failing")
        if not self.passed and not self.reasons:
            # A refusal nobody can explain is one nobody can act on.
            raise InvalidRequestError("a failing decision must say why")

    @property
    def summary(self) -> str:
        """One line, for a status field or a log."""
        return "passed the promotion gate" if self.passed else "; ".join(self.reasons)


def _pct(basis_points: int) -> str:
    """Basis points as a percentage, for a message a human reads."""
    return f"{basis_points / 100:.2f}%"
