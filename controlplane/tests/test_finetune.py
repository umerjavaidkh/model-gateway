"""Fine-tune jobs: the state machine and the reconciler that drives it.

Most of these are about money. A duplicate submission, a retried terminal job,
or a lost run each cost a GPU-hour bill that nobody notices until it arrives,
so they get more attention here than the happy path does.
"""

from __future__ import annotations

import asyncio
import os
from collections.abc import AsyncIterator

import pytest
import pytest_asyncio
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker

from model_gateway_control.contracts import run_evaluator_suite, run_trainer_suite
from model_gateway_control.db import models
from model_gateway_control.db.models import Base
from model_gateway_control.db.session import create_engine
from model_gateway_control.domain.finetune import (
    DatasetRef,
    FineTuneJob,
    Phase,
    Spec,
)
from model_gateway_control.domain.scorecard import (
    Direction,
    Metric,
    PromotionGate,
    Scorecard,
)
from model_gateway_control.errors import ConflictError, InvalidRequestError, NotFoundError
from model_gateway_control.service.evaluator import EvalPort, Target
from model_gateway_control.service.finetune import (
    Evaluators,
    FineTuneService,
    Reconciler,
    Trainers,
)
from model_gateway_control.service.trainer import Run, RunState, TrainerPort

DEFAULT_URL = "sqlite+aiosqlite:///:memory:"
CHECKSUM = "sha256:" + "a" * 64


def dataset(**overrides: object) -> DatasetRef:
    values: dict[str, object] = {
        "uri": "s3://acme-training/triage-v3.jsonl",
        "checksum": CHECKSUM,
        "rows": 48210,
        "schema_version": "chatml-v1",
    }
    values.update(overrides)
    return DatasetRef(**values)  # type: ignore[arg-type]


def spec(**overrides: object) -> Spec:
    values: dict[str, object] = {
        "tenant": "acme",
        "base_model": "llama-3.3-70b",
        "trainer": "llamafactory-lora",
        "trainer_version": "1.0.0",
        "dataset": dataset(),
        "budget_ref": "acme/training-q3",
    }
    values.update(overrides)
    return Spec(**values)  # type: ignore[arg-type]


def job(
    name: str = "support-triage-v3",
    job_spec: Spec | None = None,
    idempotency_key: str = "",
) -> FineTuneJob:
    return FineTuneJob(
        name=name,
        spec=job_spec or spec(),
        idempotency_key=idempotency_key or "key-" + name,
    )


class FakeTrainer:
    """A trainer that records what it was asked and answers as told.

    Counts submissions per idempotency key, because "was this submitted twice"
    is the question most of these tests are really asking.
    """

    def __init__(self, component: str = "llamafactory-lora") -> None:
        self._component = component
        self.submissions: list[str] = []
        self.cancelled: list[str] = []
        self.runs: dict[str, Run] = {}
        self.submit_error: Exception | None = None
        #: Accept the submission and then blow up, which is what a reconciler
        #: dying between the call and recording the answer looks like from the
        #: trainer's side: the run exists and nobody wrote down its id.
        self.crash_after_submit = False
        self.next_state = RunState.RUNNING

    def name(self) -> str:
        return self._component

    async def submit(self, job_name: str, job_spec: Spec, idempotency_key: str) -> Run:  # noqa: ARG002
        if self.submit_error is not None:
            raise self.submit_error
        self.submissions.append(idempotency_key)
        # Idempotent, as the port requires: the same key gets the same run.
        run = self.runs.setdefault(
            idempotency_key, Run(external_id=f"ext-{idempotency_key}", state=RunState.RUNNING)
        )
        if self.crash_after_submit:
            raise RuntimeError("the reconciler died after the trainer accepted the job")
        return run

    async def poll(self, external_id: str) -> Run:
        for run in self.runs.values():
            if run.external_id == external_id:
                return Run(
                    external_id=external_id,
                    state=self.next_state,
                    artifact_ref="adapters/acme/support-triage-v3"
                    if self.next_state is RunState.SUCCEEDED
                    else "",
                    cost_micro_usd=4_200_000,
                    reason="out of memory" if self.next_state is RunState.FAILED else "",
                )
        return Run(external_id=external_id, state=RunState.UNKNOWN)

    async def cancel(self, external_id: str) -> None:
        self.cancelled.append(external_id)


# --- the state machine ------------------------------------------------------


def test_a_job_records_a_submission_before_attempting_it() -> None:
    # The ordering the whole design rests on: a crash after this write and
    # before the trainer answers leaves a job whose fate is knowable.
    submitting = job().submitting()

    assert submitting.status.phase is Phase.SUBMITTING
    assert submitting.status.attempts == 1


def test_a_terminal_job_cannot_be_moved() -> None:
    # A reconciler that re-submits a finished job books a second training run,
    # and the only sign is the bill. The domain refuses rather than trusting
    # the loop to be correct.
    for finished in (
        job().submitting().submitted("ext-1").trained("adapters/a"),
        job().failed("out of memory"),
        job().cancelled(),
    ):
        with pytest.raises(ConflictError, match="cannot move to"):
            finished.submitting()


def test_a_failed_job_keeps_what_it_already_cost() -> None:
    # A run that failed after two hours on eight GPUs still cost that. Dropping
    # it makes the training budget wrong in the direction that hides overspend.
    running = job().submitting().submitted("ext-1")
    spent = running.trained("adapters/a", cost_micro_usd=4_200_000)
    assert spent.status.cost_micro_usd == 4_200_000

    failed = running.failed("out of memory")
    assert failed.status.cost_micro_usd == running.status.cost_micro_usd


def test_a_submission_with_no_external_id_is_refused() -> None:
    # Nothing could poll it, so the job would sit in TRAINING forever.
    with pytest.raises(InvalidRequestError, match="nothing can poll it"):
        job().submitting().submitted("")


def test_a_job_needs_an_idempotency_key() -> None:
    with pytest.raises(InvalidRequestError, match="paid for twice"):
        FineTuneJob(name="a-job", spec=spec(), idempotency_key="")


def test_a_dataset_must_be_pinned_by_checksum() -> None:
    # Without one, the data behind that URI can change after the job runs and
    # "which data produced this adapter" has no answer.
    with pytest.raises(InvalidRequestError, match="sha256 checksum"):
        dataset(checksum="")
    with pytest.raises(InvalidRequestError, match="must use one of"):
        dataset(uri="/local/path.jsonl")
    with pytest.raises(InvalidRequestError, match="rows"):
        dataset(rows=0)


def test_a_trainer_must_be_pinned_to_a_version() -> None:
    # An unpinned trainer turns a reproducible job into one nobody can repeat.
    with pytest.raises(InvalidRequestError, match="pinned to a version"):
        spec(trainer_version="")


# --- the reconciler ---------------------------------------------------------


@pytest.fixture
def trainer() -> FakeTrainer:
    return FakeTrainer()


@pytest.fixture
def trainers(trainer: FakeTrainer) -> Trainers:
    return Trainers((trainer,))


@pytest_asyncio.fixture
async def sessions() -> AsyncIterator[async_sessionmaker[AsyncSession]]:
    """A session factory over a fresh schema.

    A factory rather than a session, because the reconciler owns its
    transactions and needs to open its own — which is also the thing worth
    testing about it.
    """
    engine = create_engine(os.environ.get("GATEWAY_TEST_DATABASE_URL", DEFAULT_URL))
    async with engine.begin() as connection:
        await connection.run_sync(Base.metadata.drop_all)
        await connection.run_sync(Base.metadata.create_all)

    factory = async_sessionmaker(engine, expire_on_commit=False)
    async with factory() as session:
        session.add(models.Tenant(id="acme", tier="enterprise", version=1, min_trust_tier=1))
        await session.commit()

    yield factory
    await engine.dispose()


async def submit_job(sessions: async_sessionmaker[AsyncSession], trainers: Trainers) -> None:
    async with sessions() as session:
        await FineTuneService(session, trainers).submit(job())
        await session.commit()


async def test_a_job_runs_from_submission_to_a_trained_artifact(
    sessions: async_sessionmaker[AsyncSession], trainer: FakeTrainer, trainers: Trainers
) -> None:
    await submit_job(sessions, trainers)

    reconciler = Reconciler(sessions, trainers)

    # Pass one: submitted to the trainer.
    [outcome] = await reconciler.reconcile_once()
    assert outcome.job.status.phase is Phase.TRAINING
    assert outcome.job.status.external_id == "ext-key-support-triage-v3"

    # Pass two: still running, so nothing changes but the job is not lost.
    [outcome] = await reconciler.reconcile_once()
    assert outcome.job.status.phase is Phase.TRAINING
    assert outcome.advanced is False

    trainer.next_state = RunState.SUCCEEDED
    [outcome] = await reconciler.reconcile_once()
    assert outcome.job.status.phase is Phase.TRAINED
    assert outcome.job.status.artifact_ref == "adapters/acme/support-triage-v3"
    assert outcome.job.status.cost_micro_usd == 4_200_000

    # And nothing touches it again.
    assert await reconciler.reconcile_once() == []


async def test_a_crash_between_submitting_and_recording_does_not_submit_twice(
    sessions: async_sessionmaker[AsyncSession], trainer: FakeTrainer, trainers: Trainers
) -> None:
    # The expensive failure. The reconciler writes SUBMITTING, calls the
    # trainer, and dies before recording the answer. On restart it must ask
    # again with the same key rather than guess — and the port's idempotency is
    # what makes that safe.
    await submit_job(sessions, trainers)

    reconciler = Reconciler(sessions, trainers)

    # The trainer accepts, then the pass dies before it can write the external
    # id down.
    trainer.crash_after_submit = True
    assert await reconciler.reconcile_once() == []

    async with sessions() as session:
        stored = await FineTuneService(session, trainers).get("acme", "support-triage-v3")
    assert stored.status.phase is Phase.SUBMITTING

    # Restart: the same key reaches the trainer again and gets the same run.
    trainer.crash_after_submit = False
    [outcome] = await reconciler.reconcile_once()

    assert outcome.job.status.phase is Phase.TRAINING
    assert len(trainer.runs) == 1, "a second training run was booked"
    assert trainer.submissions == ["key-support-triage-v3"] * 2


async def test_a_trainer_that_raises_leaves_the_job_where_it_was(
    sessions: async_sessionmaker[AsyncSession], trainer: FakeTrainer, trainers: Trainers
) -> None:
    # A backend outage must not lose jobs or stop the loop.
    await submit_job(sessions, trainers)

    trainer.submit_error = RuntimeError("trainer is down")
    reconciler = Reconciler(sessions, trainers)

    assert await reconciler.reconcile_once() == []

    async with sessions() as session:
        stored = await FineTuneService(session, trainers).get("acme", "support-triage-v3")
    assert stored.status.phase is Phase.SUBMITTING
    assert not stored.is_terminal

    trainer.submit_error = None
    [outcome] = await reconciler.reconcile_once()
    assert outcome.job.status.phase is Phase.TRAINING


async def test_a_run_the_trainer_has_lost_fails_rather_than_restarting(
    sessions: async_sessionmaker[AsyncSession], trainer: FakeTrainer, trainers: Trainers
) -> None:
    # Silently resubmitting would book a second run against a job that may
    # still be going somewhere the backend cannot see.
    await submit_job(sessions, trainers)

    reconciler = Reconciler(sessions, trainers)
    await reconciler.reconcile_once()

    trainer.runs.clear()
    [outcome] = await reconciler.reconcile_once()

    assert outcome.job.status.phase is Phase.FAILED
    assert "no record of run" in outcome.job.status.reason


async def test_a_failed_run_records_what_it_burned(
    sessions: async_sessionmaker[AsyncSession], trainer: FakeTrainer, trainers: Trainers
) -> None:
    await submit_job(sessions, trainers)

    reconciler = Reconciler(sessions, trainers)
    await reconciler.reconcile_once()
    trainer.next_state = RunState.FAILED

    [outcome] = await reconciler.reconcile_once()

    assert outcome.job.status.phase is Phase.FAILED
    assert outcome.job.status.reason == "out of memory"
    assert outcome.job.status.cost_micro_usd == 4_200_000


async def test_cancelling_tells_the_trainer(
    sessions: async_sessionmaker[AsyncSession], trainer: FakeTrainer, trainers: Trainers
) -> None:
    await submit_job(sessions, trainers)

    await Reconciler(sessions, trainers).reconcile_once()

    async with sessions() as session:
        cancelled = await FineTuneService(session, trainers).cancel("acme", "support-triage-v3")
        await session.commit()

    assert cancelled.status.phase is Phase.CANCELLED
    assert trainer.cancelled == ["ext-key-support-triage-v3"]
    # And no pass picks it up again.
    assert await Reconciler(sessions, trainers).reconcile_once() == []


async def test_cancelling_a_finished_job_is_not_an_error(
    sessions: async_sessionmaker[AsyncSession], trainer: FakeTrainer, trainers: Trainers
) -> None:
    # An operator cancelling something that just finished wanted it stopped,
    # and it is stopped.
    await submit_job(sessions, trainers)

    reconciler = Reconciler(sessions, trainers)
    await reconciler.reconcile_once()
    trainer.next_state = RunState.SUCCEEDED
    await reconciler.reconcile_once()

    async with sessions() as session:
        settled = await FineTuneService(session, trainers).cancel("acme", "support-triage-v3")

    assert settled.status.phase is Phase.TRAINED
    assert trainer.cancelled == []


async def test_an_unknown_trainer_is_refused_at_submission(
    sessions: async_sessionmaker[AsyncSession], trainers: Trainers
) -> None:
    # Now, while someone is watching — rather than two minutes later inside a
    # loop nobody is reading the logs of.
    async with sessions() as session:
        with pytest.raises(NotFoundError, match="no trainer named"):
            await FineTuneService(session, trainers).submit(job(job_spec=spec(trainer="nope")))


async def test_a_duplicate_job_name_is_refused(
    sessions: async_sessionmaker[AsyncSession], trainers: Trainers
) -> None:
    async with sessions() as session:
        service = FineTuneService(session, trainers)
        await service.submit(job())
        with pytest.raises(ConflictError, match="already exists"):
            await service.submit(job())


def test_two_trainers_cannot_share_a_name() -> None:
    # Which one ran would depend on ordering, and a job's spec names one.
    with pytest.raises(InvalidRequestError, match="two trainers named"):
        Trainers((FakeTrainer(), FakeTrainer()))


# --- the trainer contract ---------------------------------------------------


async def test_the_fake_trainer_satisfies_the_trainer_contract() -> None:
    # The suite an adapter for a real backend must pass. Running it against the
    # fake keeps the fake honest — a fake that does not satisfy the contract
    # makes every test above prove something about a trainer that could not
    # exist.
    async def build() -> TrainerPort:
        return FakeTrainer()

    await run_trainer_suite(build)


async def test_the_contract_suite_catches_a_trainer_that_is_not_idempotent() -> None:
    # The suite's own load-bearing case. If this passed a non-idempotent
    # trainer, the suite would be documentation rather than a gate.
    class DuplicatingTrainer(FakeTrainer):
        _counter = 0

        async def submit(self, job_name: str, job_spec: Spec, idempotency_key: str) -> Run:  # noqa: ARG002
            DuplicatingTrainer._counter += 1
            return Run(external_id=f"ext-{DuplicatingTrainer._counter}", state=RunState.RUNNING)

    async def build() -> TrainerPort:
        return DuplicatingTrainer()

    with pytest.raises(AssertionError, match="not idempotent"):
        await run_trainer_suite(build)


async def test_the_contract_suite_catches_a_trainer_that_hides_unknown_runs() -> None:
    # Reporting an unknown run as failed would make the reconciler mark jobs
    # terminal for runs that are still going.
    class ConfidentTrainer(FakeTrainer):
        async def poll(self, external_id: str) -> Run:
            return Run(external_id=external_id, state=RunState.FAILED, reason="probably")

    async def build() -> TrainerPort:
        return ConfidentTrainer()

    with pytest.raises(AssertionError, match="cannot distinguish"):
        await run_trainer_suite(build)


@pytest.mark.skipif(
    "sqlite" in os.environ.get("GATEWAY_TEST_DATABASE_URL", DEFAULT_URL),
    reason="SELECT ... FOR UPDATE SKIP LOCKED needs a real database; SQLite ignores it",
)
async def test_two_reconcilers_book_only_one_training_run(
    sessions: async_sessionmaker[AsyncSession], trainer: FakeTrainer, trainers: Trainers
) -> None:
    # What makes running several replicas a capacity decision rather than a
    # correctness one — and it is the idempotency key that does it, not a lock.
    # Making submission durable means committing before calling the trainer,
    # and that commit releases any lock the transaction held, so a second pass
    # genuinely can call submit for the same job.
    #
    # It may therefore submit more than once. What it must never do is produce
    # more than one run, because a run is what costs money.
    await submit_job(sessions, trainers)

    first = Reconciler(sessions, trainers)
    second = Reconciler(sessions, trainers)
    await asyncio.gather(first.reconcile_once(), second.reconcile_once())

    assert len(trainer.runs) == 1, f"two reconcilers booked {len(trainer.runs)} training runs"
    assert set(trainer.submissions) == {"key-support-triage-v3"}, (
        "a submission went out under a key that was not this job's"
    )

    async with sessions() as session:
        stored = await FineTuneService(session, trainers).get("acme", "support-triage-v3")
    assert stored.status.phase is Phase.TRAINING
    assert stored.status.external_id == "ext-key-support-triage-v3"


# --- the eval gate ----------------------------------------------------------


class FakeEvaluator:
    """A suite that reports what it is told to, and records what it was asked."""

    def __init__(self, suite: str = "triage-regression-v2", version: str = "1.0.0") -> None:
        self._suite = suite
        self._version = version
        self.targets: list[Target] = []
        self.run_error: Exception | None = None
        self.candidate_score = 9_100
        self.baseline_score = 9_000
        self.candidate_latency = 1_100
        self.baseline_latency = 1_200

    def name(self) -> str:
        return self._suite

    def version(self) -> str:
        return self._version

    async def run(self, target: Target) -> Scorecard:
        if self.run_error is not None:
            raise self.run_error
        self.targets.append(target)
        score = self.baseline_score if target.describes_baseline else self.candidate_score
        ms = self.baseline_latency if target.describes_baseline else self.candidate_latency
        return Scorecard(
            score=score,
            suite=self._suite,
            suite_version=self._version,
            metrics=(
                Metric(
                    name="latency_p95",
                    value=ms,
                    direction=Direction.LOWER_IS_BETTER,
                    unit="ms",
                ),
            ),
        )


@pytest.fixture
def evaluator() -> FakeEvaluator:
    return FakeEvaluator()


@pytest.fixture
def evaluators(evaluator: FakeEvaluator) -> Evaluators:
    return Evaluators((evaluator,))


def gated_spec(**gate_kwargs: object) -> Spec:
    return spec(
        eval_suite="triage-regression-v2",
        promotion_gate=PromotionGate(**gate_kwargs),  # type: ignore[arg-type]
    )


async def train_to_evaluating(
    sessions: async_sessionmaker[AsyncSession],
    trainers: Trainers,
    evaluators: Evaluators,
    trainer: FakeTrainer,
    job_spec: Spec,
) -> Reconciler:
    """Run a job as far as EVALUATING and return the reconciler."""
    async with sessions() as session:
        await FineTuneService(session, trainers, evaluators).submit(job(job_spec=job_spec))
        await session.commit()

    reconciler = Reconciler(sessions, trainers, evaluators)
    await reconciler.reconcile_once()
    trainer.next_state = RunState.SUCCEEDED
    await reconciler.reconcile_once()
    return reconciler


async def test_a_job_with_no_eval_suite_stops_at_trained(
    sessions: async_sessionmaker[AsyncSession], trainer: FakeTrainer, trainers: Trainers
) -> None:
    # An artifact nobody has measured is one an operator promotes deliberately,
    # not one the loop promotes because no gate happened to be configured.
    await submit_job(sessions, trainers)
    reconciler = Reconciler(sessions, trainers)
    await reconciler.reconcile_once()
    trainer.next_state = RunState.SUCCEEDED

    [outcome] = await reconciler.reconcile_once()

    assert outcome.job.status.phase is Phase.TRAINED
    assert outcome.job.is_terminal
    assert await reconciler.reconcile_once() == []


async def test_an_artifact_that_clears_the_gate_becomes_ready(
    sessions: async_sessionmaker[AsyncSession],
    trainer: FakeTrainer,
    trainers: Trainers,
    evaluator: FakeEvaluator,
    evaluators: Evaluators,
) -> None:
    reconciler = await train_to_evaluating(
        sessions, trainers, evaluators, trainer, gated_spec(min_score=8_700)
    )

    [outcome] = await reconciler.reconcile_once()

    assert outcome.job.status.phase is Phase.READY
    assert outcome.job.status.scorecard is not None
    assert outcome.job.status.scorecard.score == 9_100
    # No baseline was needed, so none was measured: a second evaluation for a
    # gate that is purely a minimum score doubles its cost to learn nothing.
    assert [t.describes_baseline for t in evaluator.targets] == [False]


async def test_an_artifact_that_misses_the_bar_fails_and_keeps_its_numbers(
    sessions: async_sessionmaker[AsyncSession],
    trainer: FakeTrainer,
    trainers: Trainers,
    evaluator: FakeEvaluator,
    evaluators: Evaluators,
) -> None:
    # A failed gate is exactly when someone wants to see what was measured.
    # Discarding it means the only way to find out is to train again.
    evaluator.candidate_score = 5_000
    reconciler = await train_to_evaluating(
        sessions, trainers, evaluators, trainer, gated_spec(min_score=8_700)
    )

    [outcome] = await reconciler.reconcile_once()

    assert outcome.job.status.phase is Phase.FAILED
    assert outcome.job.status.scorecard is not None
    assert outcome.job.status.scorecard.score == 5_000
    assert "below the required" in outcome.job.status.reason


async def test_a_baseline_is_measured_only_when_something_must_not_regress(
    sessions: async_sessionmaker[AsyncSession],
    trainer: FakeTrainer,
    trainers: Trainers,
    evaluator: FakeEvaluator,
    evaluators: Evaluators,
) -> None:
    reconciler = await train_to_evaluating(
        sessions, trainers, evaluators, trainer, gated_spec(must_not_regress=("latency_p95",))
    )

    [outcome] = await reconciler.reconcile_once()

    assert outcome.job.status.phase is Phase.READY
    # The candidate, then the base model, measured by the same suite version.
    assert [t.describes_baseline for t in evaluator.targets] == [False, True]
    assert outcome.job.status.baseline is not None
    assert outcome.job.status.baseline.suite_version == "1.0.0"


async def test_a_regression_against_the_base_model_fails_the_gate(
    sessions: async_sessionmaker[AsyncSession],
    trainer: FakeTrainer,
    trainers: Trainers,
    evaluator: FakeEvaluator,
    evaluators: Evaluators,
) -> None:
    # The failure a fine-tune actually produces: no errors, just worse output.
    evaluator.candidate_latency = 2_000
    evaluator.baseline_latency = 1_000
    reconciler = await train_to_evaluating(
        sessions, trainers, evaluators, trainer, gated_spec(must_not_regress=("latency_p95",))
    )

    [outcome] = await reconciler.reconcile_once()

    assert outcome.job.status.phase is Phase.FAILED
    assert "latency_p95 regressed from 1000 to 2000" in outcome.job.status.reason


async def test_scorecards_survive_the_round_trip(
    sessions: async_sessionmaker[AsyncSession],
    trainer: FakeTrainer,
    trainers: Trainers,
    evaluators: Evaluators,
) -> None:
    # A scorecard that loses its direction on the way to the database makes
    # every later comparison meaningless.
    reconciler = await train_to_evaluating(
        sessions, trainers, evaluators, trainer, gated_spec(must_not_regress=("latency_p95",))
    )
    await reconciler.reconcile_once()

    async with sessions() as session:
        stored = await FineTuneService(session, trainers, evaluators).get(
            "acme", "support-triage-v3"
        )

    assert stored.status.scorecard is not None
    measured = stored.status.scorecard.metric("latency_p95")
    assert measured is not None
    assert measured.direction is Direction.LOWER_IS_BETTER
    assert measured.unit == "ms"
    assert stored.spec.promotion_gate.must_not_regress == ("latency_p95",)


async def test_an_evaluator_that_raises_leaves_the_job_evaluating(
    sessions: async_sessionmaker[AsyncSession],
    trainer: FakeTrainer,
    trainers: Trainers,
    evaluator: FakeEvaluator,
    evaluators: Evaluators,
) -> None:
    # An eval backend outage must not fail an artifact that trained fine.
    reconciler = await train_to_evaluating(
        sessions, trainers, evaluators, trainer, gated_spec(min_score=8_700)
    )

    evaluator.run_error = RuntimeError("the eval harness is down")
    assert await reconciler.reconcile_once() == []

    async with sessions() as session:
        stored = await FineTuneService(session, trainers, evaluators).get(
            "acme", "support-triage-v3"
        )
    assert stored.status.phase is Phase.EVALUATING
    assert not stored.is_terminal

    # And it recovers once the harness is back.
    evaluator.run_error = None
    [outcome] = await reconciler.reconcile_once()
    assert outcome.job.status.phase is Phase.READY


async def test_a_job_naming_an_unconfigured_suite_is_refused_before_it_trains(
    sessions: async_sessionmaker[AsyncSession], trainers: Trainers, evaluators: Evaluators
) -> None:
    # A job whose gate can never run would train, at full cost, and then stall.
    async with sessions() as session:
        with pytest.raises(NotFoundError, match="no eval suite named"):
            await FineTuneService(session, trainers, evaluators).submit(
                job(job_spec=spec(eval_suite="a-suite-nobody-configured"))
            )


async def test_the_fake_evaluator_satisfies_the_evaluator_contract() -> None:
    # Keeps the fake honest: one that does not satisfy the contract makes every
    # test above prove something about a suite that could not exist.
    async def build() -> EvalPort:
        return FakeEvaluator()

    await run_evaluator_suite(build)


async def test_the_contract_suite_catches_an_unstamped_scorecard() -> None:
    # The gate refuses a candidate and a baseline from different suite
    # versions. An unstamped scorecard makes that check pass by accident.
    class AnonymousEvaluator(FakeEvaluator):
        async def run(self, target: Target) -> Scorecard:
            card = await super().run(target)
            return Scorecard(score=card.score, metrics=card.metrics)

    async def build() -> EvalPort:
        return AnonymousEvaluator()

    with pytest.raises(AssertionError, match="scorecard says suite"):
        await run_evaluator_suite(build)


# --- weighted rollout -------------------------------------------------------


async def ready_job(
    sessions: async_sessionmaker[AsyncSession],
    trainers: Trainers,
    evaluators: Evaluators,
    trainer: FakeTrainer,
) -> None:
    """Run a job all the way to READY."""
    reconciler = await train_to_evaluating(
        sessions, trainers, evaluators, trainer, gated_spec(min_score=8_700)
    )
    await reconciler.reconcile_once()


def test_an_adapter_starts_at_zero_traffic() -> None:
    # A fine-tuned regression is silent — no errors, just worse output — so the
    # adapter enters the routing table and takes nothing until an operator says
    # otherwise.
    started = _ready().start_rollout()

    assert started.rolling_out
    assert started.status.rollout_weight == 0
    assert started.status.rollout_step == 0


def test_the_walk_follows_the_declared_steps() -> None:
    walking = _ready().start_rollout()

    weights = []
    for _ in range(4):
        walking = walking.advance_rollout()
        weights.append(walking.status.rollout_weight)

    assert weights == [1, 5, 25, 100]

    with pytest.raises(ConflictError, match="no further steps"):
        walking.advance_rollout()


def test_an_abort_returns_the_adapter_to_zero_without_removing_it() -> None:
    # It stays in the routing table: removing it would mean the next rollout
    # starts from nothing, losing the record that this one happened — and an
    # aborted rollout is exactly what someone will want to find later.
    aborted = _ready().start_rollout().advance_rollout().advance_rollout().abort_rollout()

    assert aborted.status.rollout_weight == 0
    assert aborted.rolling_out
    assert "aborted" in aborted.status.reason


def test_only_an_artifact_that_cleared_its_gate_can_roll_out() -> None:
    trained = job(job_spec=spec()).submitting().submitted("ext-1").trained("adapters/a")

    with pytest.raises(ConflictError, match="cleared its gate"):
        trained.start_rollout()


def test_a_rollout_cannot_be_started_twice() -> None:
    with pytest.raises(ConflictError, match="already rolling out"):
        _ready().start_rollout().start_rollout()


def test_advancing_without_a_rollout_is_refused() -> None:
    with pytest.raises(ConflictError, match="no rollout to advance"):
        _ready().advance_rollout()


def test_canary_steps_must_ascend() -> None:
    # A rollout that goes backwards is a rollback, and a rollback is a snapshot
    # version away rather than a step.
    with pytest.raises(InvalidRequestError, match="must ascend"):
        spec(canary_steps=(25, 5))
    with pytest.raises(InvalidRequestError, match="percentage above zero"):
        spec(canary_steps=(0, 50))
    with pytest.raises(InvalidRequestError, match="percentage above zero"):
        spec(canary_steps=(50, 200))


def _ready() -> FineTuneJob:
    """A job that has cleared its gate, built without touching a database."""
    from model_gateway_control.domain.scorecard import Decision

    return (
        job(job_spec=gated_spec(min_score=8_700))
        .submitting()
        .submitted("ext-1")
        .trained("adapters/acme/support-triage-v3")
        .evaluated(Decision(passed=True), Scorecard(score=9_100, suite="s", suite_version="1"))
    )


async def test_a_rollout_survives_the_round_trip(
    sessions: async_sessionmaker[AsyncSession],
    trainer: FakeTrainer,
    trainers: Trainers,
    evaluators: Evaluators,
) -> None:
    await ready_job(sessions, trainers, evaluators, trainer)

    async with sessions() as session:
        service = FineTuneService(session, trainers, evaluators)
        await service.start_rollout("acme", "support-triage-v3")
        await service.advance_rollout("acme", "support-triage-v3")
        await session.commit()

    async with sessions() as session:
        stored = await FineTuneService(session, trainers, evaluators).get(
            "acme", "support-triage-v3"
        )

    assert stored.status.rollout_weight == 1
    assert stored.status.rollout_step == 1
    assert stored.spec.canary_steps == (1, 5, 25, 100)
