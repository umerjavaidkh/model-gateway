"""TrainerPort: the seam between a fine-tune job and whatever runs it.

A control-plane port, with a different discipline from the four in the data
plane. Its work is asynchronous and artifact-producing, its latency is measured
in hours, and it never executes inside a request. What it shares with them is
that it is a contract maintained forever, so it is deliberately four methods.

# The contract that costs money if it is wrong

``submit`` must be idempotent on ``idempotency_key``. Called twice with the
same key, it must return the same run rather than starting a second one.

That is not a nicety. The reconciler writes down that it is about to submit,
calls submit, and writes down the answer. A crash between the call and the
answer leaves a job whose fate is unknown, and the only safe recovery is to
call submit again with the same key and be told about the run that already
exists. A backend without that property cannot be made safe from the outside:
the alternative is guessing, and a wrong guess books a second training run on
eight GPUs that nobody is watching.

An adapter for a backend that has no idempotency of its own has to build it —
usually by tagging the run with the key and searching before creating. Doing
that badly is still better than not doing it, because the failure mode of not
doing it is a duplicate bill.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import StrEnum
from typing import Protocol

from model_gateway_control.domain.finetune import Spec


class RunState(StrEnum):
    """What a trainer says about a run it is holding."""

    RUNNING = "running"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    #: The backend has no record of it. Distinct from failed: a run that never
    #: started can be started, and one that failed must not be silently retried
    #: as though it had not.
    UNKNOWN = "unknown"


@dataclass(frozen=True, slots=True, kw_only=True)
class Run:
    """A training run as the backend describes it."""

    external_id: str
    state: RunState
    #: Where the adapter ended up. Set only once the run succeeded.
    artifact_ref: str = ""
    #: What it has cost so far, integer micro-USD. Reported as it accrues, so a
    #: run that fails halfway still accounts for what it burned.
    cost_micro_usd: int = 0
    #: Why it failed, for a failed run.
    reason: str = ""


class TrainerPort(Protocol):
    """A backend that can run a fine-tune."""

    def name(self) -> str:
        """The component name this trainer is registered under."""

    async def submit(self, job_name: str, spec: Spec, idempotency_key: str) -> Run:
        """Start a run, or return the one this key already started.

        Must be idempotent on ``idempotency_key``. See the module docstring:
        this is the property the reconciler's crash safety rests on, and there
        is no way to add it from the outside.
        """

    async def poll(self, external_id: str) -> Run:
        """Ask about a run the backend is holding."""

    async def cancel(self, external_id: str) -> None:
        """Stop a run. Must be safe to call on one that already stopped."""
