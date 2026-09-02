"""The contract every EvalPort must satisfy.

Shorter than the trainer's, because an eval suite has less it can get wrong
from the outside — but what it can get wrong is worse. A trainer that
misbehaves costs money; a suite that misbehaves promotes a regression into the
routing pool, which is the thing the gate exists to prevent.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable

from model_gateway_control.domain.scorecard import BASIS_POINTS
from model_gateway_control.service.evaluator import EvalPort, Target

#: Builds a suite to test. A factory so each case gets a clean one.
EvaluatorFactory = Callable[[], Awaitable[EvalPort]]

CANDIDATE = Target(base_model="llama-3.3-70b", artifact_ref="adapters/contract/candidate")
BASELINE = Target(base_model="llama-3.3-70b")


async def run_evaluator_suite(new_evaluator: EvaluatorFactory) -> None:
    """Assert the behaviour every EvalPort must have.

    Raises AssertionError on the first failure, so it drops into whatever test
    runner an adapter uses without this module having to know about one.
    """
    await _reports_a_stable_name_and_version(new_evaluator)
    await _stamps_its_identity_on_every_scorecard(new_evaluator)
    await _measures_a_baseline_the_same_way(new_evaluator)
    await _is_repeatable(new_evaluator)


async def _reports_a_stable_name_and_version(new_evaluator: EvaluatorFactory) -> None:
    # A job's spec names a suite, so a name that changes between constructions
    # silently unbinds every job that named it.
    first, second = await new_evaluator(), await new_evaluator()
    assert first.name(), "a suite with no name cannot be named in a job spec"
    assert first.name() == second.name(), "the suite name is not stable"
    assert first.version(), (
        "a suite with no version makes every scorecard incomparable, because "
        "the gate cannot tell whether two runs measured the same thing"
    )
    assert first.version() == second.version(), "the suite version is not stable"


async def _stamps_its_identity_on_every_scorecard(new_evaluator: EvaluatorFactory) -> None:
    # The gate compares a candidate against a baseline and refuses if they came
    # from different suite versions. An unstamped scorecard makes that check
    # pass by accident.
    suite = await new_evaluator()

    card = await suite.run(CANDIDATE)

    assert card.suite == suite.name(), (
        f"scorecard says suite {card.suite!r} but the suite is {suite.name()!r}"
    )
    assert card.suite_version == suite.version(), (
        f"scorecard says version {card.suite_version!r} but the suite is {suite.version()!r}"
    )
    assert 0 <= card.score <= BASIS_POINTS, (
        f"score {card.score} is not a basis-point value; a suite reporting a "
        "fraction or a percentage will be compared against a gate in basis points"
    )


async def _measures_a_baseline_the_same_way(new_evaluator: EvaluatorFactory) -> None:
    # A baseline is the base model measured by the same suite. If it reports a
    # different metric set, every must-not-regress check fails as "missing"
    # rather than telling anyone anything.
    suite = await new_evaluator()

    candidate = await suite.run(CANDIDATE)
    baseline = await suite.run(BASELINE)

    assert {m.name for m in baseline.metrics} == {m.name for m in candidate.metrics}, (
        "the suite reports different metrics for a baseline than for a candidate, "
        "so no regression can be checked"
    )
    for measured in candidate.metrics:
        before = baseline.metric(measured.name)
        assert before is not None
        assert before.direction is measured.direction, (
            f"metric {measured.name!r} points one way on a candidate and the other "
            "on a baseline, so a regression would read as an improvement"
        )


async def _is_repeatable(new_evaluator: EvaluatorFactory) -> None:
    # Not "deterministic" — a suite may sample. But the shape has to hold, or
    # the gate is comparing different measurements each run.
    suite = await new_evaluator()

    first = await suite.run(CANDIDATE)
    second = await suite.run(CANDIDATE)

    assert {m.name for m in first.metrics} == {m.name for m in second.metrics}, (
        "two runs of the same suite against the same target reported different metrics"
    )
