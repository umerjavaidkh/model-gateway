"""The fine-tune reconciler: what moves a job from a spec to an artifact.

A loop, not a workflow engine. Each pass takes one job, looks at where it is,
does the single next thing, and writes down what happened. A job that needs
five steps takes five passes, and a pass that crashes leaves the job somewhere
a later pass can pick it up from — which is the property that matters, because
the alternative is a half-finished orchestration nobody can resume.

# The two failures worth designing for

**A crash between submitting and recording the answer.** The reconciler writes
SUBMITTING *before* calling the trainer, so a job found in that phase on
restart is one whose fate is unknown. It resolves that by calling submit again
with the same idempotency key — the TrainerPort contract requires that to
return the existing run rather than starting a second — instead of guessing.
Guessing wrong books a second run on eight GPUs that nobody is watching.

**Two reconcilers picking up the same job.** Handled by the idempotency key,
not by a lock, and it is worth being exact about why: making submission
durable means committing the SUBMITTING row *before* calling the trainer, and
committing releases any lock the transaction held. No row lock can span an
external call that has to be preceded by a commit.

So a second reconciler genuinely can call submit for the same job, and the
port's idempotency requirement is what makes that cost nothing — one run, not
two. The row lock in _advance still earns its place by keeping two passes from
interleaving writes to one job's status, and by letting a replica skip a job
someone else is already working rather than duplicating the effort. It is not
what makes the design safe.
"""

from __future__ import annotations

import json
import logging
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from datetime import UTC, datetime

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker

from model_gateway_control.db import models
from model_gateway_control.domain.finetune import (
    TERMINAL,
    DatasetRef,
    FineTuneJob,
    Phase,
    Spec,
    Status,
)
from model_gateway_control.domain.scorecard import (
    Direction,
    Metric,
    PromotionGate,
    Scorecard,
)
from model_gateway_control.errors import ConflictError, InvalidRequestError, NotFoundError
from model_gateway_control.service.evaluator import EvalPort, Target
from model_gateway_control.service.rollout import Policy as RolloutPolicy
from model_gateway_control.service.rollout import RolloutHealth
from model_gateway_control.service.trainer import RunState, TrainerPort

logger = logging.getLogger(__name__)

#: Where "now" comes from, injected so tests do not sleep.
Clock = Callable[[], datetime]

#: Phases the reconciler has work to do in. Terminal jobs are never claimed,
#: so a finished job cannot be acted on by a loop that has a bug.
ACTIONABLE = (Phase.PENDING, Phase.SUBMITTING, Phase.TRAINING, Phase.EVALUATING)


@dataclass(frozen=True, slots=True)
class Outcome:
    """What one reconciler pass did, for logs and tests."""

    job: FineTuneJob
    #: True when this pass changed the phase. A pass that polled a still-running
    #: job did useful work and changed nothing, and the two read differently in
    #: a log.
    advanced: bool


class Evaluators:
    """The eval suites this control plane can run, by name.

    Resolved by name rather than executed from whatever a tenant supplied: a
    suite is code that produces numbers a gate then trusts, so one that always
    returns a perfect score promotes everything. Binding these to the component
    registry — signed, admitted and sandboxed like any other plugin — is the
    natural next step and is one change for this and Trainers together.
    """

    def __init__(self, evaluators: Sequence[EvalPort] = ()) -> None:
        by_name: dict[str, EvalPort] = {}
        for evaluator in evaluators:
            name = evaluator.name()
            if not name:
                raise InvalidRequestError("an eval suite reported an empty name")
            if name in by_name:
                raise InvalidRequestError(f"two eval suites named {name!r}")
            by_name[name] = evaluator
        self._by_name = by_name

    def resolve(self, name: str) -> EvalPort:
        """The suite registered under this name."""
        evaluator = self._by_name.get(name)
        if evaluator is None:
            raise NotFoundError(
                f"no eval suite named {name!r} is registered with this control plane"
            )
        return evaluator


class Trainers:
    """The trainers this control plane can submit to, by component name.

    Resolved by name because a job's spec names a trainer component, and that
    component is versioned, contract-tested and admitted through the registry
    like any other plugin — rather than being a forked script nobody can audit.
    """

    def __init__(self, trainers: Sequence[TrainerPort] = ()) -> None:
        by_name: dict[str, TrainerPort] = {}
        for trainer in trainers:
            name = trainer.name()
            if not name:
                raise InvalidRequestError("a trainer reported an empty name")
            if name in by_name:
                raise InvalidRequestError(f"two trainers named {name!r}")
            by_name[name] = trainer
        self._by_name = by_name

    def resolve(self, name: str) -> TrainerPort:
        """The trainer registered under this name."""
        trainer = self._by_name.get(name)
        if trainer is None:
            raise NotFoundError(f"no trainer named {name!r} is registered with this control plane")
        return trainer


class FineTuneService:
    """Submitting, reading and cancelling jobs, from a request.

    Takes a request-scoped session and never commits, like every other service
    here: the caller owns the transaction. The reconciler below is the
    exception, and it says why.
    """

    def __init__(
        self,
        session: AsyncSession,
        trainers: Trainers | None = None,
        evaluators: Evaluators | None = None,
        now: Clock | None = None,
    ) -> None:
        self._session = session
        self._trainers = trainers or Trainers()
        self._evaluators = evaluators or Evaluators()
        self._now = now or _utcnow

    async def submit(self, job: FineTuneJob) -> FineTuneJob:
        """Accept a new job. It is not sent anywhere until a pass picks it up.

        Accepting and starting are separate on purpose: the caller gets a fast,
        durable answer, and the expensive part happens where it can be retried.
        """
        if job.status.phase is not Phase.PENDING:
            raise InvalidRequestError(
                f"job {job.ref} was submitted already in phase {job.status.phase}"
            )
        existing = await self._find(job.spec.tenant, job.name)
        if existing is not None:
            raise ConflictError(
                f"job {job.ref} already exists; submit a new job rather than reusing a name"
            )

        # Named before they are trusted: both have to exist now, not when a
        # reconciler pass discovers they do not two minutes later. A job whose
        # gate can never run would train — at full cost — and then stall.
        self._trainers.resolve(job.spec.trainer)
        if job.spec.eval_suite:
            self._evaluators.resolve(job.spec.eval_suite)

        row = models.FineTuneJob(
            tenant_id=job.spec.tenant,
            name=job.name,
            base_model=job.spec.base_model,
            trainer=job.spec.trainer,
            trainer_version=job.spec.trainer_version,
            dataset_uri=job.spec.dataset.uri,
            dataset_checksum=job.spec.dataset.checksum,
            dataset_rows=job.spec.dataset.rows,
            dataset_schema_version=job.spec.dataset.schema_version,
            hyperparameters=json.dumps(job.spec.hyperparameters, sort_keys=True),
            budget_ref=job.spec.budget_ref,
            eval_suite=job.spec.eval_suite,
            gate_min_score=job.spec.promotion_gate.min_score,
            gate_must_not_regress=json.dumps(list(job.spec.promotion_gate.must_not_regress)),
            canary_steps=json.dumps(list(job.spec.canary_steps)),
            shadow_percent=job.spec.shadow_percent,
            idempotency_key=job.idempotency_key,
            phase=str(Phase.PENDING),
        )
        self._session.add(row)
        await self._session.flush()
        return to_job(row)

    async def get(self, tenant: str, name: str) -> FineTuneJob:
        """One job."""
        return to_job(await self._require(tenant, name))

    async def list(self, tenant: str | None = None) -> list[FineTuneJob]:
        """Every job, newest first."""
        query = select(models.FineTuneJob).order_by(models.FineTuneJob.id.desc())
        if tenant is not None:
            query = query.where(models.FineTuneJob.tenant_id == tenant)
        return [to_job(row) for row in (await self._session.scalars(query)).all()]

    async def start_rollout(self, tenant: str, name: str) -> FineTuneJob:
        """Put a cleared artifact into the routing table at zero traffic."""
        return await self._rollout(tenant, name, lambda job: job.start_rollout())

    async def advance_rollout(self, tenant: str, name: str) -> FineTuneJob:
        """Take the next canary step."""
        return await self._rollout(tenant, name, lambda job: job.advance_rollout())

    async def abort_rollout(self, tenant: str, name: str, reason: str = "") -> FineTuneJob:
        """Return the adapter to zero traffic."""
        return await self._rollout(
            tenant, name, lambda job: job.abort_rollout(reason or "aborted by an operator")
        )

    async def _rollout(
        self, tenant: str, name: str, step: Callable[[FineTuneJob], FineTuneJob]
    ) -> FineTuneJob:
        row = await self._require(tenant, name)
        return self._store(row, step(to_job(row)))

    async def cancel(self, tenant: str, name: str) -> FineTuneJob:
        """Stop a job, telling the trainer if it has one."""
        row = await self._require(tenant, name)
        job = to_job(row)
        if job.is_terminal:
            # Not an error. An operator cancelling something that just finished
            # wanted it stopped, and it is stopped.
            return job

        if job.status.external_id:
            trainer = self._trainers.resolve(job.spec.trainer)
            await trainer.cancel(job.status.external_id)

        return self._store(row, job.cancelled())

    def _store(self, row: models.FineTuneJob, job: FineTuneJob) -> FineTuneJob:
        _apply_status(row, job.status, self._now())
        return to_job(row)

    async def _find(self, tenant: str, name: str) -> models.FineTuneJob | None:
        row: models.FineTuneJob | None = await self._session.scalar(
            select(models.FineTuneJob).where(
                models.FineTuneJob.tenant_id == tenant, models.FineTuneJob.name == name
            )
        )
        return row

    async def _require(self, tenant: str, name: str) -> models.FineTuneJob:
        row = await self._find(tenant, name)
        if row is None:
            raise NotFoundError(f"no fine-tune job {tenant}/{name}")
        return row


class Reconciler:
    """Advances fine-tune jobs one step at a time.

    Unlike the other services here, this one owns its transactions. It has to:
    the whole point of writing SUBMITTING before calling a trainer is that the
    write is durable when the call happens, and a service that leaves
    committing to its caller cannot promise that. So it takes a session factory
    rather than a session, and each pass is its own transaction.
    """

    def __init__(
        self,
        sessions: async_sessionmaker[AsyncSession],
        trainers: Trainers | None = None,
        evaluators: Evaluators | None = None,
        now: Clock | None = None,
        health: RolloutPolicy | None = None,
    ) -> None:
        self._sessions = sessions
        self._trainers = trainers or Trainers()
        self._evaluators = evaluators or Evaluators()
        self._now = now or _utcnow
        # None disables automatic advancement entirely: a deployment that wants
        # an operator to walk every step gets exactly that, rather than an
        # automation it has to remember to turn off.
        self._health = health

    async def advance_rollouts(self) -> list[Outcome]:
        """Move canaries a step, or take them out, on what traffic did.

        A separate pass from reconcile_once because it answers a different
        question about a different set of jobs: those are jobs on their way to
        an artifact, these are artifacts on their way into production. Folding
        them together would mean a training backlog delayed a rollout decision
        and a stuck rollout looked like a training problem.

        Health is measured, never assumed. A rollout with no evidence waits
        rather than advancing — see service/rollout.py for why the three
        outcomes are advance, abort and wait rather than a boolean.
        """
        if self._health is None:
            return []

        async with self._sessions() as session:
            candidates = [
                row.id
                for row in (
                    await session.scalars(
                        select(models.FineTuneJob).where(
                            models.FineTuneJob.rollout_step >= 0,
                            models.FineTuneJob.rollout_weight < 100,
                        )
                    )
                ).all()
            ]

        outcomes = []
        for job_id in candidates:
            outcome = await self._advance_rollout(job_id)
            if outcome is not None:
                outcomes.append(outcome)
        return outcomes

    async def _advance_rollout(self, job_id: int) -> Outcome | None:
        async with self._sessions() as session:
            row = await session.get(
                models.FineTuneJob, job_id, with_for_update={"skip_locked": True}
            )
            if row is None or row.rollout_step < 0 or row.rollout_weight >= 100:
                return None

            job = to_job(row)
            health = self._health_for(session)
            # updated_at is when this step began: every status write sets it,
            # and a step is a status write.
            started_at = row.updated_at or self._now()

            try:
                verdict = await health.judge(
                    adapter=f"{row.tenant_id}-{row.name}",
                    base=row.base_model,
                    started_at=started_at,
                    now=self._now(),
                )
            except Exception as exc:
                # A rollout that cannot be judged is left where it is. Guessing
                # would mean advancing on no evidence or aborting on a database
                # blip, and both are worse than waiting.
                logger.warning("rollout for %s could not be judged: %s", job.ref, exc)
                return None

            if verdict.wait:
                logger.debug("rollout for %s waiting: %s", job.ref, verdict.reason)
                return None

            advanced = (
                job.advance_rollout() if verdict.advance else job.abort_rollout(verdict.reason)
            )
            _apply_status(row, advanced.status, self._now())
            await session.commit()

            logger.info(
                "rollout for %s %s: %s",
                job.ref,
                "advanced" if verdict.advance else "aborted",
                verdict.reason,
            )
            return Outcome(job=advanced, advanced=True)

    def _health_for(self, session: AsyncSession) -> RolloutHealth:
        return RolloutHealth(session, self._health)

    async def reconcile_once(self) -> list[Outcome]:
        """Advance every job that has work outstanding.

        Returns what it did, so a caller can log it and a test can assert on it
        without reaching into the database.
        """
        async with self._sessions() as session:
            pending = [row.id for row in await _needing_work(session)]

        outcomes = []
        for job_id in pending:
            outcome = await self._advance(job_id)
            if outcome is not None:
                outcomes.append(outcome)
        return outcomes

    async def _advance(self, job_id: int) -> Outcome | None:
        """Do the one next thing for a job, in its own transaction.

        ``skip_locked`` so a second reconciler moves on to the next job rather
        than queueing behind this one. The lock keeps two passes from
        interleaving writes to one job's status; it cannot stop two passes from
        both submitting, because the commit that makes SUBMITTING durable
        releases it before the trainer is ever called. That case is the
        idempotency key's job.
        """
        async with self._sessions() as session:
            row = await session.get(
                models.FineTuneJob, job_id, with_for_update={"skip_locked": True}
            )
            if row is None or Phase(row.phase) in TERMINAL:
                # Either another reconciler holds it, or it finished since the
                # selection. Neither is an error.
                return None

            job = to_job(row)
            before = job.status.phase

            if job.status.phase is Phase.PENDING:
                # Committed before the trainer is called, and that ordering is
                # the reason this class owns its transactions at all. A crash
                # after the commit and before the call leaves a job in
                # SUBMITTING, which the next pass resolves by asking the
                # trainer about the idempotency key rather than guessing.
                #
                # It also releases the row lock, which is why the key rather
                # than the lock is what stops a concurrent pass paying twice.
                job = job.submitting()
                _apply_status(row, job.status, self._now())
                await session.commit()

            try:
                job = await self._step(job)
            except Exception as exc:
                # A trainer that raises must not stop the loop or lose the job.
                # It stays where it is and the next pass tries again; only the
                # domain decides when something is terminal.
                logger.warning("job %s could not be advanced: %s", job.ref, exc)
                await session.rollback()
                return None

            _apply_status(row, job.status, self._now())
            advanced = job.status.phase is not before
            await session.commit()
            return Outcome(job=job, advanced=advanced)

    async def _step(self, job: FineTuneJob) -> FineTuneJob:
        if job.status.phase is Phase.EVALUATING:
            return await self._evaluate(job)

        trainer = self._trainers.resolve(job.spec.trainer)
        if job.status.phase is Phase.SUBMITTING:
            run = await trainer.submit(job.name, job.spec, job.idempotency_key)
            if run.state is RunState.FAILED:
                return job.failed(run.reason or "the trainer refused the job")
            return job.submitted(run.external_id)

        run = await trainer.poll(job.status.external_id)

        if run.state is RunState.SUCCEEDED:
            return job.trained(run.artifact_ref, run.cost_micro_usd)
        if run.state is RunState.FAILED:
            return job.failed(run.reason or "the trainer reported a failure", run.cost_micro_usd)
        if run.state is RunState.UNKNOWN:
            # The backend has lost a run it told us about. Failing is the
            # honest answer: silently resubmitting would book a second run
            # against a job that may still be going.
            return job.failed(f"the trainer has no record of run {job.status.external_id!r}")

        # Still running — or in a state a later version of this enum adds and
        # this one does not know. Waiting is the safe default either way: it
        # does nothing and the next pass asks again, whereas guessing at a
        # meaning could mark a running job terminal, and a terminal job is one
        # nothing will ever collect the artifact from.
        return job

    async def _evaluate(self, job: FineTuneJob) -> FineTuneJob:
        """Measure the artifact, and the base model if anything must not regress.

        Both in one pass rather than one per pass. An eval run is minutes
        against a training run's hours, and splitting it would mean carrying a
        half-finished comparison across a crash — where the risk is not cost
        but a gate decided against a baseline measured by a different version
        of the suite.
        """
        suite = self._evaluators.resolve(job.spec.eval_suite)
        gate = job.spec.promotion_gate

        candidate = await suite.run(
            Target(base_model=job.spec.base_model, artifact_ref=job.status.artifact_ref)
        )
        # Only when the gate needs something to compare against: running a
        # second evaluation for a gate that is purely a minimum score would
        # double its cost to learn nothing.
        baseline = (
            await suite.run(Target(base_model=job.spec.base_model)) if gate.needs_baseline else None
        )

        return job.evaluated(gate.decide(candidate, baseline), candidate, baseline)


async def _needing_work(session: AsyncSession) -> Sequence[models.FineTuneJob]:
    """Which jobs have something outstanding.

    Deliberately unlocked. The rows are re-read under a lock in _advance, and
    a lock taken here would have to be released before that — a lock released
    before the work it guards protects nothing, and holding one across every
    job in a pass would serialise the whole loop.

    So this is a hint, and it is allowed to be stale: a job that finished since
    the query is caught by the terminal check, and one another reconciler is
    already holding is skipped by the lock.
    """
    query = (
        select(models.FineTuneJob)
        .where(models.FineTuneJob.phase.in_([str(p) for p in ACTIONABLE]))
        .order_by(models.FineTuneJob.id)
    )
    return (await session.scalars(query)).all()


def _apply_status(row: models.FineTuneJob, status: Status, when: datetime) -> None:
    row.phase = str(status.phase)
    row.external_id = status.external_id
    row.artifact_ref = status.artifact_ref
    row.reason = status.reason
    row.cost_micro_usd = status.cost_micro_usd
    row.attempts = status.attempts
    row.rollout_step = status.rollout_step
    row.rollout_weight = status.rollout_weight
    row.scorecard = encode_scorecard(status.scorecard)
    row.baseline = encode_scorecard(status.baseline)
    row.updated_at = when


def encode_scorecard(card: Scorecard | None) -> str:
    """A scorecard as the JSON a row stores. Empty for none."""
    if card is None:
        return ""
    return json.dumps(
        {
            "score": card.score,
            "suite": card.suite,
            "suite_version": card.suite_version,
            "metrics": [
                {
                    "name": m.name,
                    "value": m.value,
                    "direction": str(m.direction),
                    "unit": m.unit,
                }
                for m in card.metrics
            ],
        },
        sort_keys=True,
    )


def decode_scorecard(raw: str) -> Scorecard | None:
    """A scorecard from what a row stores."""
    if not raw:
        return None
    decoded = json.loads(raw)
    return Scorecard(
        score=decoded["score"],
        suite=decoded.get("suite", ""),
        suite_version=decoded.get("suite_version", ""),
        metrics=tuple(
            Metric(
                name=m["name"],
                value=m["value"],
                direction=Direction(m["direction"]),
                unit=m.get("unit", ""),
            )
            for m in decoded.get("metrics", [])
        ),
    )


def job_key(job: FineTuneJob) -> tuple[str, str]:
    """How a job is identified within a tenant."""
    return (job.spec.tenant, job.name)


def to_job(row: models.FineTuneJob) -> FineTuneJob:
    """Map a job row to its domain form."""
    return FineTuneJob(
        name=row.name,
        idempotency_key=row.idempotency_key,
        created_at=row.created_at,
        spec=Spec(
            tenant=row.tenant_id,
            base_model=row.base_model,
            trainer=row.trainer,
            trainer_version=row.trainer_version,
            dataset=DatasetRef(
                uri=row.dataset_uri,
                checksum=row.dataset_checksum,
                rows=row.dataset_rows,
                schema_version=row.dataset_schema_version,
            ),
            hyperparameters=json.loads(row.hyperparameters or "{}"),
            budget_ref=row.budget_ref,
            eval_suite=row.eval_suite,
            promotion_gate=PromotionGate(
                min_score=row.gate_min_score,
                must_not_regress=tuple(json.loads(row.gate_must_not_regress or "[]")),
            ),
            canary_steps=tuple(json.loads(row.canary_steps or "[]")),
            shadow_percent=row.shadow_percent,
        ),
        status=Status(
            phase=Phase(row.phase),
            external_id=row.external_id,
            artifact_ref=row.artifact_ref,
            reason=row.reason,
            cost_micro_usd=row.cost_micro_usd,
            attempts=row.attempts,
            rollout_step=row.rollout_step,
            rollout_weight=row.rollout_weight,
            scorecard=decode_scorecard(row.scorecard),
            baseline=decode_scorecard(row.baseline),
            updated_at=row.updated_at,
        ),
    )


def _utcnow() -> datetime:
    return datetime.now(UTC)


__all__ = [
    "ACTIONABLE",
    "TERMINAL",
    "Evaluators",
    "FineTuneService",
    "Outcome",
    "Reconciler",
    "Trainers",
    "to_job",
]
