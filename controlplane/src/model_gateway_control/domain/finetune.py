"""Fine-tuning jobs: what is asked for, and where a job has got to.

Declarative on purpose. A job is a ``spec`` an operator or an agent submits and
a ``status`` a control-plane loop maintains, because the alternative — a client
orchestrating upload, submit, poll, commit and recovering when step four fails
— is the shape that gets abandoned half-done. Here the client states the
desired end and the reconciler is responsible for reaching it.

# The two things that make this different from the rest of the control plane

**Every step costs real money and some of it cannot be undone.** A single
fine-tune can exceed a month of inference. That makes ordinary reconciler
sloppiness — a duplicate submission after a crash, a retry of a terminal job —
expensive rather than merely untidy, and it is why submission is written down
before it is attempted and why terminal states are enforced in the domain
rather than trusted to the loop.

**The gateway never holds training data.** A dataset is a pointer: URI,
checksum, row count, schema version. The gateway validates the reference and
hands it to a trainer; it does not read, copy, or store the rows.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field, replace
from datetime import datetime
from enum import StrEnum
from uuid import uuid4

from model_gateway_control.domain.scorecard import Decision, PromotionGate, Scorecard
from model_gateway_control.errors import ConflictError, InvalidRequestError

#: The rollout the plan describes: 1%, then 5, 25 and 100. A default rather
#: than a requirement — a tenant with little traffic learns nothing from 1%.
DEFAULT_CANARY_STEPS = (1, 5, 25, 100)

_NAME = re.compile(r"^[a-z0-9][a-z0-9-]{2,63}$")
_CHECKSUM = re.compile(r"^sha256:[0-9a-f]{64}$")

#: Schemes a dataset may live behind. The gateway never fetches these itself —
#: the trainer does — but an unreachable or nonsensical scheme is worth
#: refusing at submission rather than discovering after a GPU has been booked.
_DATASET_SCHEMES = ("s3://", "gs://", "https://", "file://")


class Phase(StrEnum):
    """Where a job has got to.

    The plan's phase list, with one addition: SUBMITTING. It exists because the
    gap between "we are about to tell the trainer to start" and "we know what
    the trainer called it" is the one place a crash costs money — see
    FineTuneJob.submitting.
    """

    #: Accepted, not yet sent anywhere.
    PENDING = "pending"
    #: A submission is in flight. Recorded before the call, not after.
    SUBMITTING = "submitting"
    #: The trainer has it and is working.
    TRAINING = "training"
    #: Training produced an artifact. Not yet allowed to serve: whether it may
    #: is the eval gate's decision.
    TRAINED = "trained"
    #: The suite is running against the artifact, and against the base model
    #: too when the gate has something that must not regress.
    EVALUATING = "evaluating"
    #: Cleared the gate. Eligible to enter the routing pool — actually entering
    #: it is a rollout, which is weighted rather than a flip.
    READY = "ready"
    #: Terminal. Reached from any non-terminal phase.
    FAILED = "failed"
    #: Terminal, and operator-initiated.
    CANCELLED = "cancelled"


#: Phases from which nothing further happens. Enforced in the domain rather
#: than in the reconciler: a loop that re-submits a finished job books a second
#: GPU run, and "the reconciler has a bug" is not a good reason for that to be
#: possible.
TERMINAL = frozenset({Phase.TRAINED, Phase.READY, Phase.FAILED, Phase.CANCELLED})


@dataclass(frozen=True, slots=True, kw_only=True)
class DatasetRef:
    """A pointer to training data the gateway does not hold."""

    uri: str
    #: Of the dataset contents. The trainer verifies it; the gateway records it
    #: so that "which data produced this adapter" has an answer that survives
    #: someone overwriting the object at that URI.
    checksum: str
    rows: int
    schema_version: str

    def __post_init__(self) -> None:
        if not self.uri.startswith(_DATASET_SCHEMES):
            raise InvalidRequestError(
                f"dataset {self.uri!r} must use one of {', '.join(_DATASET_SCHEMES)}"
            )
        if not _CHECKSUM.match(self.checksum):
            raise InvalidRequestError(
                f"dataset {self.uri!r} needs a sha256 checksum; "
                "without one, the data behind that URI can change after the job runs"
            )
        if self.rows <= 0:
            raise InvalidRequestError(f"dataset {self.uri!r} declares {self.rows} rows")
        if not self.schema_version:
            raise InvalidRequestError(f"dataset {self.uri!r} needs a schema version")


@dataclass(frozen=True, slots=True, kw_only=True)
class Spec:
    """What was asked for. Immutable once a job starts."""

    tenant: str
    base_model: str
    #: The trainer component to use, resolved through the component registry so
    #: a trainer is versioned, contract-tested and admitted like any other
    #: plugin rather than being a forked script nobody can audit.
    trainer: str
    trainer_version: str
    dataset: DatasetRef
    #: Passed through to the trainer as-is. The gateway does not interpret
    #: them: every backend has its own, and a schema here would be a
    #: lowest-common-denominator that blocks the interesting ones.
    hyperparameters: dict[str, str] = field(default_factory=dict)
    #: Training spend is its own budget dimension, never a line item under the
    #: inference budget. One fine-tune can exceed a month of serving, and a
    #: shared budget means training silently starves inference or the reverse.
    budget_ref: str = ""
    #: What must vouch for the artifact before it can serve. Empty means
    #: nothing does, and the job stops at TRAINED — an artifact nobody has
    #: measured is one an operator promotes deliberately, not one the loop
    #: promotes because no gate was configured.
    eval_suite: str = ""
    #: The bar that suite must clear. Recorded on the spec so the bar a job
    #: faces is fixed when it is submitted: lowering the gate afterwards cannot
    #: retroactively promote something that already failed it.
    promotion_gate: PromotionGate = field(default_factory=PromotionGate)
    #: Traffic shares to walk through, as percentages. A fine-tuned model's
    #: regression is silent — no errors, just worse output — so the adapter
    #: enters the routing table at zero and climbs, rather than being switched
    #: on. Rollback is free: it is snapshot version N-1.
    canary_steps: tuple[int, ...] = DEFAULT_CANARY_STEPS

    def __post_init__(self) -> None:
        if not self.tenant:
            raise InvalidRequestError("a fine-tune job needs a tenant")
        if not self.base_model:
            raise InvalidRequestError("a fine-tune job needs a base model")
        if not self.trainer:
            raise InvalidRequestError("a fine-tune job needs a trainer component")
        if self.canary_steps:
            if list(self.canary_steps) != sorted(self.canary_steps):
                raise InvalidRequestError(
                    f"canary steps {self.canary_steps} must ascend; a rollout that goes "
                    "backwards is a rollback, and a rollback is a snapshot away"
                )
            if not all(0 < step <= 100 for step in self.canary_steps):
                raise InvalidRequestError(
                    f"canary steps {self.canary_steps} must each be a percentage above zero"
                )
        if not self.trainer_version:
            # Pinned for the same reason a component image is: an unpinned
            # trainer turns a reproducible job into one nobody can repeat.
            raise InvalidRequestError(f"trainer {self.trainer!r} must be pinned to a version")


@dataclass(frozen=True, slots=True, kw_only=True)
class Status:
    """Where the job has got to. Maintained by the reconciler."""

    phase: Phase = Phase.PENDING
    #: What the trainer calls this run. Absent until the trainer answers.
    external_id: str = ""
    #: Where the trained adapter ended up. Absent until training finishes.
    artifact_ref: str = ""
    #: Why it failed, for a terminal failure — or why the gate passed.
    reason: str = ""
    #: What the suite measured about the artifact.
    scorecard: Scorecard | None = None
    #: What it measured about the base model, when the gate needed something to
    #: compare against.
    baseline: Scorecard | None = None
    #: The share of traffic the adapter is currently taking, as a percentage.
    #: Zero means it is in the routing table and not serving, which is where a
    #: rollout starts and where an abort returns it.
    rollout_weight: int = 0
    #: How far through the canary steps it has walked. -1 means no rollout has
    #: been started, which is distinct from step 0 at weight 0.
    rollout_step: int = -1
    #: What the run cost, in integer micro-USD. Money is never a float.
    cost_micro_usd: int = 0
    #: How many times the reconciler has acted on this job. An operator asking
    #: "is this stuck" needs to distinguish a job nothing has touched from one
    #: being retried in a loop.
    attempts: int = 0
    updated_at: datetime | None = None

    def __post_init__(self) -> None:
        if self.cost_micro_usd < 0:
            raise InvalidRequestError("a job cannot have cost a negative amount")
        if not 0 <= self.rollout_weight <= 100:
            raise InvalidRequestError(f"rollout weight {self.rollout_weight} is not a percentage")


@dataclass(frozen=True, slots=True, kw_only=True)
class FineTuneJob:
    """A fine-tuning job: what was asked for, and where it has got to."""

    name: str
    spec: Spec
    status: Status = field(default_factory=Status)
    #: Generated once, at creation, and sent with every submission attempt.
    #:
    #: This is what makes a crash between "submit" and "record the answer"
    #: survivable. Without it, a reconciler that dies after the trainer accepts
    #: a job but before the external id is written has no way to tell, on
    #: restart, whether to submit again — and submitting again books a second
    #: GPU run that nobody is watching and everybody pays for.
    idempotency_key: str
    created_at: datetime | None = None

    @staticmethod
    def new_key() -> str:
        """A fresh idempotency key.

        Generated here rather than accepted from a client. A caller-supplied
        key that collided with another job's would make an idempotent trainer
        hand both jobs the same run — the second would silently adopt the
        first's training and its artifact, and both would look successful.
        """
        return uuid4().hex

    def __post_init__(self) -> None:
        if not _NAME.match(self.name):
            raise InvalidRequestError(
                f"job name {self.name!r} must be a lowercase slug of 3 to 64 characters"
            )
        if not self.idempotency_key:
            raise InvalidRequestError(
                f"job {self.name!r} has no idempotency key; a submission that cannot be "
                "recognised on retry is one that can be paid for twice"
            )

    @property
    def ref(self) -> str:
        """How a job is named in a log line and an error."""
        return f"{self.spec.tenant}/{self.name}"

    @property
    def is_terminal(self) -> bool:
        """Whether anything further will happen to this job."""
        return self.status.phase in TERMINAL

    def submitting(self) -> FineTuneJob:
        """Record that a submission is about to be attempted.

        Written *before* the trainer is called, and that ordering is the whole
        point. A job found in SUBMITTING on restart is one whose fate is
        unknown: it may have reached the trainer, it may not. The reconciler
        resolves that by asking the trainer about the idempotency key rather
        than by guessing, which it can only do because this row exists.
        """
        return self._advance(Phase.SUBMITTING, attempts=self.status.attempts + 1)

    def submitted(self, external_id: str) -> FineTuneJob:
        """Record that the trainer has the job and what it calls it."""
        if not external_id:
            raise InvalidRequestError(
                f"job {self.ref} was submitted but the trainer returned no id, "
                "so nothing can poll it"
            )
        return self._advance(Phase.TRAINING, external_id=external_id)

    def trained(self, artifact_ref: str, cost_micro_usd: int = 0) -> FineTuneJob:
        """Record a finished training run and where its artifact went.

        Lands in EVALUATING when the spec names a suite, and in TRAINED — which
        is terminal — when it does not. An artifact nobody has measured is one
        an operator promotes deliberately, rather than one the loop promotes
        because no gate happened to be configured.
        """
        if not artifact_ref:
            raise InvalidRequestError(
                f"job {self.ref} finished training with no artifact reference"
            )
        return self._advance(
            Phase.EVALUATING if self.spec.eval_suite else Phase.TRAINED,
            artifact_ref=artifact_ref,
            cost_micro_usd=cost_micro_usd,
        )

    def evaluated(
        self, decision: Decision, scorecard: Scorecard, baseline: Scorecard | None = None
    ) -> FineTuneJob:
        """Record the gate's verdict and the numbers behind it.

        The scorecards are kept either way. A failed gate is the case where
        someone most wants to see what was measured, and discarding it means
        the only way to find out is to train again.
        """
        phase = Phase.READY if decision.passed else Phase.FAILED
        return self._advance(phase, reason=decision.summary, scorecard=scorecard, baseline=baseline)

    def failed(self, reason: str, cost_micro_usd: int | None = None) -> FineTuneJob:
        """Record a terminal failure.

        Cost is carried over by default: a run that failed after two hours on
        eight GPUs still cost two hours on eight GPUs, and dropping that makes
        the training budget wrong in the direction that hides overspend.
        """
        return self._advance(
            Phase.FAILED,
            reason=reason or "no reason given",
            cost_micro_usd=(
                self.status.cost_micro_usd if cost_micro_usd is None else cost_micro_usd
            ),
        )

    # --- rollout ----------------------------------------------------------
    #
    # Weighted, not a flip. A fine-tuned model's regression is silent, so the
    # adapter enters the routing table at zero, climbs through the steps, and
    # can be returned to zero at any point. Each of these is an operator's
    # decision rather than a timer's: without a health signal to advance on,
    # a rollout that promoted itself would promote a bad adapter just as
    # reliably as a good one.

    @property
    def rolling_out(self) -> bool:
        """Whether a rollout has been started for this artifact."""
        return self.status.rollout_step >= 0

    @property
    def adapter_id(self) -> str:
        """How the routing key names this adapter.

        The job name, which is already unique within a tenant. The routing key
        is (base model, adapter), so one base deployment serves every adapter
        trained from it — which is the whole economics of multi-LoRA.
        """
        return self.name

    def start_rollout(self) -> FineTuneJob:
        """Put the adapter in the routing table at weight zero."""
        if self.status.phase is not Phase.READY:
            raise ConflictError(
                f"job {self.ref} is {self.status.phase}; only an artifact that cleared "
                "its gate can enter the routing table"
            )
        if self.rolling_out:
            raise ConflictError(f"job {self.ref} is already rolling out")
        return replace(self, status=replace(self.status, rollout_step=0, rollout_weight=0))

    def advance_rollout(self) -> FineTuneJob:
        """Take the next canary step."""
        if not self.rolling_out:
            raise ConflictError(f"job {self.ref} has no rollout to advance")

        steps = self.spec.canary_steps
        nxt = self.status.rollout_step
        if nxt >= len(steps):
            raise ConflictError(
                f"job {self.ref} is at {self.status.rollout_weight}% and has no further steps"
            )
        return replace(
            self, status=replace(self.status, rollout_step=nxt + 1, rollout_weight=steps[nxt])
        )

    def abort_rollout(self, reason: str = "aborted by an operator") -> FineTuneJob:
        """Return the adapter to zero traffic.

        It stays in the routing table rather than being removed. Removing it
        would mean the next rollout starts from nothing, losing the record that
        this one happened — and an aborted rollout is exactly the thing someone
        will want to find later.
        """
        if not self.rolling_out:
            raise ConflictError(f"job {self.ref} has no rollout to abort")
        return replace(
            self, status=replace(self.status, rollout_weight=0, rollout_step=0, reason=reason)
        )

    def cancelled(self, reason: str = "cancelled by an operator") -> FineTuneJob:
        """Record an operator stopping the job."""
        return self._advance(Phase.CANCELLED, reason=reason)

    def _advance(
        self,
        phase: Phase,
        *,
        external_id: str | None = None,
        artifact_ref: str | None = None,
        reason: str | None = None,
        cost_micro_usd: int | None = None,
        attempts: int | None = None,
        scorecard: Scorecard | None = None,
        baseline: Scorecard | None = None,
    ) -> FineTuneJob:
        """Move to a phase, carrying forward whatever this step does not set.

        Spelled out rather than taking ``**kwargs``, because the alternative
        needs a type: ignore — and the one method that decides whether a job
        can be submitted twice is not where to save six lines of typing.
        """
        if self.is_terminal:
            # The expensive mistake. A reconciler that re-submits a finished
            # job books a second training run, and the only sign is the bill.
            raise ConflictError(f"job {self.ref} is {self.status.phase} and cannot move to {phase}")

        current = self.status
        return replace(
            self,
            status=replace(
                current,
                phase=phase,
                external_id=current.external_id if external_id is None else external_id,
                artifact_ref=current.artifact_ref if artifact_ref is None else artifact_ref,
                reason=current.reason if reason is None else reason,
                cost_micro_usd=(
                    current.cost_micro_usd if cost_micro_usd is None else cost_micro_usd
                ),
                attempts=current.attempts if attempts is None else attempts,
                scorecard=current.scorecard if scorecard is None else scorecard,
                baseline=current.baseline if baseline is None else baseline,
            ),
        )
