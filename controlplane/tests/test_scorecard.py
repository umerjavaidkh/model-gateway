"""The promotion gate: what it passes, what it refuses, and why.

The gate is what makes agent-driven promotion safe — an agent can promote
without a human reading the numbers, provided the bar means exactly one thing.
These are about the ways a bar can quietly mean nothing.
"""

from __future__ import annotations

import pytest

from model_gateway_control.domain.scorecard import (
    BASIS_POINTS,
    Direction,
    Metric,
    PromotionGate,
    Scorecard,
)
from model_gateway_control.errors import InvalidRequestError

SUITE = "triage-regression-v2"


def latency(ms: int) -> Metric:
    return Metric(name="latency_p95", value=ms, direction=Direction.LOWER_IS_BETTER, unit="ms")


def accuracy(basis_points: int) -> Metric:
    return Metric(name="accuracy", value=basis_points, direction=Direction.HIGHER_IS_BETTER)


def card(score: int, *metrics: Metric, version: str = "1.0.0") -> Scorecard:
    return Scorecard(score=score, metrics=metrics, suite=SUITE, suite_version=version)


# --- the minimum score ------------------------------------------------------


def test_a_score_at_the_bar_passes_and_below_it_does_not() -> None:
    gate = PromotionGate(min_score=8_700)

    assert gate.decide(card(8_700)).passed
    assert gate.decide(card(8_701)).passed
    assert not gate.decide(card(8_699)).passed


def test_the_comparison_is_exact() -> None:
    # The reason scores are basis points. As floats, 0.87 against 0.8699999999
    # is decided by the last bits of a double, and the same comparison can go
    # differently on two machines.
    gate = PromotionGate(min_score=8_700)

    decision = gate.decide(card(8_699))

    assert not decision.passed
    assert "86.99%" in decision.summary
    assert "87.00%" in decision.summary


def test_a_failing_decision_says_why() -> None:
    # A refusal nobody can explain is one nobody can act on, and an agent
    # driving promotion has nothing else to read.
    decision = PromotionGate(min_score=9_000).decide(card(5_000))

    assert not decision.passed
    assert decision.reasons


# --- regressions ------------------------------------------------------------


def test_lower_is_better_metrics_regress_upward() -> None:
    # The direction the plan's example leaves implicit. Getting it backwards
    # does not fail loudly: it passes exactly the regressions the gate exists
    # to catch.
    gate = PromotionGate(must_not_regress=("latency_p95",))
    baseline = card(9_000, latency(1_200))

    assert gate.decide(card(9_000, latency(1_100)), baseline).passed
    assert not gate.decide(card(9_000, latency(1_300)), baseline).passed


def test_higher_is_better_metrics_regress_downward() -> None:
    gate = PromotionGate(must_not_regress=("accuracy",))
    baseline = card(9_000, accuracy(8_000))

    assert gate.decide(card(9_000, accuracy(8_100)), baseline).passed
    assert not gate.decide(card(9_000, accuracy(7_900)), baseline).passed


def test_every_reason_is_reported_not_just_the_first() -> None:
    # A publisher looking at a rejected adapter wants to know everything that
    # has to change, rather than one thing per training run.
    gate = PromotionGate(min_score=9_500, must_not_regress=("latency_p95", "accuracy"))
    baseline = card(9_000, latency(1_000), accuracy(8_000))
    candidate = card(9_000, latency(1_500), accuracy(7_000))

    decision = gate.decide(candidate, baseline)

    assert not decision.passed
    assert len(decision.reasons) == 3
    assert any("score" in r for r in decision.reasons)
    assert any("latency_p95 regressed" in r for r in decision.reasons)
    assert any("accuracy regressed" in r for r in decision.reasons)


def test_a_missing_baseline_fails_rather_than_passes() -> None:
    # A gate that silently passes when the thing it compares against is missing
    # is a gate that stops working the moment a baseline run fails.
    gate = PromotionGate(must_not_regress=("latency_p95",))

    decision = gate.decide(card(9_900, latency(10)), baseline=None)

    assert not decision.passed
    assert "no baseline" in decision.summary


def test_a_baseline_from_a_different_suite_version_is_refused() -> None:
    # Comparing against a scorecard from a different version of the suite is
    # comparing two different measurements. Answering anyway would produce a
    # verdict whose meaning depends on which run happened to be older.
    gate = PromotionGate(must_not_regress=("latency_p95",))
    baseline = card(9_000, latency(1_000), version="1.0.0")
    candidate = card(9_000, latency(900), version="2.0.0")

    decision = gate.decide(candidate, baseline)

    assert not decision.passed
    assert "not the same measurement" in decision.summary


def test_a_metric_missing_from_either_side_fails() -> None:
    gate = PromotionGate(must_not_regress=("latency_p95",))

    decision = gate.decide(card(9_000), card(9_000, latency(1_000)))

    assert not decision.passed
    assert "missing" in decision.summary


def test_a_metric_that_disagrees_about_its_own_direction_is_refused() -> None:
    # Two suites, or two versions of one, describing the same name differently.
    # Answering would produce a verdict whose meaning depends on which side was
    # asked.
    rising = Metric(name="score", value=10, direction=Direction.HIGHER_IS_BETTER)
    falling = Metric(name="score", value=10, direction=Direction.LOWER_IS_BETTER)

    with pytest.raises(InvalidRequestError, match="one of them is wrong"):
        rising.is_worse_than(falling)


def test_a_gate_with_nothing_in_it_passes_everything() -> None:
    # Legitimate for a job whose promotion is a human decision. It is also why
    # a job with no eval suite stops at TRAINED rather than being promoted:
    # an empty gate must never be mistaken for enforcement.
    assert PromotionGate().decide(card(0)).passed


# --- construction -----------------------------------------------------------


def test_scores_outside_the_basis_point_range_are_refused() -> None:
    # A suite reporting a percentage or a fraction rather than basis points
    # would otherwise be compared against a gate on the wrong scale.
    with pytest.raises(InvalidRequestError, match="basis-point"):
        Scorecard(score=BASIS_POINTS + 1)
    with pytest.raises(InvalidRequestError, match="basis-point"):
        Scorecard(score=-1)
    with pytest.raises(InvalidRequestError, match="basis-point"):
        PromotionGate(min_score=20_000)


def test_a_scorecard_cannot_report_a_metric_twice() -> None:
    # Which one the gate read would depend on ordering.
    with pytest.raises(InvalidRequestError, match="twice"):
        Scorecard(score=9_000, metrics=(latency(10), latency(20)))


def test_a_gate_cannot_name_a_metric_twice() -> None:
    with pytest.raises(InvalidRequestError, match="twice"):
        PromotionGate(must_not_regress=("latency_p95", "latency_p95"))


def test_a_decision_must_be_coherent() -> None:
    from model_gateway_control.domain.scorecard import Decision

    with pytest.raises(InvalidRequestError, match="must say why"):
        Decision(passed=False)
    with pytest.raises(InvalidRequestError, match="cannot carry reasons"):
        Decision(passed=True, reasons=("but",))
