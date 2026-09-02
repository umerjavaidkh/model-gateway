"""The contract every TrainerPort must satisfy.

Importable by an adapter's own tests, so a backend integration is checked
against the same battery wherever it lives. The alternative — each adapter
asserting what it thinks the contract is — is how two adapters end up disagreeing
about whether ``submit`` may start a second run.

The suite is deliberately mostly about idempotency and about what a trainer
says when it has lost a run. Those are the two behaviours the reconciler's
crash safety rests on, and they are the two an adapter author is most likely to
assume rather than implement.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable

from model_gateway_control.domain.finetune import DatasetRef, Spec
from model_gateway_control.service.trainer import RunState, TrainerPort

#: Builds a trainer to test, and a spec it will accept. A factory rather than an
#: instance so each case gets a clean one: a suite that shares a trainer between
#: cases cannot tell "submit is idempotent" from "the second case reused the
#: first one's run".
TrainerFactory = Callable[[], Awaitable[TrainerPort]]


def sample_spec(tenant: str = "acme") -> Spec:
    """A spec any trainer should accept, for suites that need one."""
    return Spec(
        tenant=tenant,
        base_model="llama-3.3-70b",
        trainer="contract-suite",
        trainer_version="1.0.0",
        dataset=DatasetRef(
            uri="s3://contract/dataset.jsonl",
            checksum="sha256:" + "0" * 64,
            rows=100,
            schema_version="chatml-v1",
        ),
    )


async def run_trainer_suite(new_trainer: TrainerFactory, spec: Spec | None = None) -> None:
    """Assert the behaviour every TrainerPort must have.

    Raises AssertionError on the first failure, so it drops into whatever test
    runner an adapter uses without this module having to know about one.
    """
    spec = spec or sample_spec()

    await _reports_a_stable_name(new_trainer)
    await _submitting_twice_starts_one_run(new_trainer, spec)
    await _a_submitted_run_is_pollable(new_trainer, spec)
    await _an_unknown_run_is_unknown_not_failed(new_trainer)
    await _cancelling_is_safe_to_repeat(new_trainer, spec)


async def _reports_a_stable_name(new_trainer: TrainerFactory) -> None:
    # A job's spec names its trainer, so a name that changes between
    # constructions silently unbinds every job that named it.
    first, second = await new_trainer(), await new_trainer()
    assert first.name(), "a trainer with no name cannot be named in a job spec"
    assert first.name() == second.name(), (
        f"name is not stable: {first.name()!r} then {second.name()!r}"
    )


async def _submitting_twice_starts_one_run(new_trainer: TrainerFactory, spec: Spec) -> None:
    # The property the reconciler's crash safety rests on. A reconciler that
    # dies between submitting and recording the answer recovers by submitting
    # again with the same key; if that starts a second run, the recovery costs
    # a second GPU bill.
    trainer = await new_trainer()

    first = await trainer.submit("contract-job", spec, "contract-key-1")
    second = await trainer.submit("contract-job", spec, "contract-key-1")

    assert first.external_id, "submit returned no external id, so nothing can poll the run"
    assert second.external_id == first.external_id, (
        "submit is not idempotent: the same idempotency key produced "
        f"{first.external_id!r} and then {second.external_id!r}, which is two training runs"
    )


async def _a_submitted_run_is_pollable(new_trainer: TrainerFactory, spec: Spec) -> None:
    # A run the trainer just acknowledged must be one it can be asked about.
    # Otherwise the reconciler moves a job to TRAINING and then immediately
    # fails it for a run the backend claims not to have.
    trainer = await new_trainer()
    submitted = await trainer.submit("contract-job", spec, "contract-key-2")

    polled = await trainer.poll(submitted.external_id)

    assert polled.external_id == submitted.external_id
    assert polled.state is not RunState.UNKNOWN, (
        "a run the trainer just accepted polls as unknown, so the reconciler "
        "would fail every job it submits"
    )


async def _an_unknown_run_is_unknown_not_failed(new_trainer: TrainerFactory) -> None:
    # The distinction matters: a run that never existed can be started, and one
    # that failed must not be silently retried as though it had not.
    trainer = await new_trainer()

    polled = await trainer.poll("a-run-that-was-never-submitted")

    assert polled.state is RunState.UNKNOWN, (
        f"polling a run that does not exist reported {polled.state}, "
        "which the reconciler cannot distinguish from a real outcome"
    )


async def _cancelling_is_safe_to_repeat(new_trainer: TrainerFactory, spec: Spec) -> None:
    # The reconciler and an operator can both cancel, and a cancel that races
    # a finishing run must not raise.
    trainer = await new_trainer()
    submitted = await trainer.submit("contract-job", spec, "contract-key-3")

    await trainer.cancel(submitted.external_id)
    await trainer.cancel(submitted.external_id)
    await trainer.cancel("a-run-that-was-never-submitted")
