"""EvalPort: the seam between a promotion gate and whatever measures a model.

The second control-plane port, and the one the plan's open question is about —
who owns eval suites. A suite is code that runs against a model and produces
numbers a gate then trusts, which makes it exactly as security-relevant as a
guardrail: a suite that always returns 0.99 promotes everything.

So this is a port with a contract suite of its own, in the same shape as
TrainerPort, and a deployment resolves a suite by name rather than executing
whatever a tenant supplied. Binding these to the component registry — so a
suite is signed, admitted and sandboxed like any other plugin — is the next
step and is one change for both ports rather than one for each.

# The contract

``run`` must be deterministic enough to compare. The gate's whole premise is
that a candidate's numbers and a baseline's numbers mean the same thing, and
that fails if the suite reports a different scale, a different metric set, or a
different direction for the same metric between two runs. The contract suite
checks the parts of that which are checkable from outside.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Protocol

from model_gateway_control.domain.scorecard import Scorecard


@dataclass(frozen=True, slots=True, kw_only=True)
class Target:
    """What is being evaluated.

    A base model with no adapter is how a baseline is measured, which is why
    the adapter is optional rather than there being two methods: the point of a
    baseline is that it was measured the same way as the candidate.
    """

    base_model: str
    #: Where the fine-tuned adapter is, empty for a baseline run.
    artifact_ref: str = ""

    @property
    def describes_baseline(self) -> bool:
        """Whether this target is the unmodified base model."""
        return not self.artifact_ref


class EvalPort(Protocol):
    """A suite that can measure a model."""

    def name(self) -> str:
        """The suite name a job's spec refers to."""

    def version(self) -> str:
        """Which version of the suite this is.

        Stamped onto every scorecard it produces, because a candidate compared
        against a baseline from a different version is comparing two different
        measurements — and the gate refuses that rather than reporting a
        meaningless verdict.
        """

    async def run(self, target: Target) -> Scorecard:
        """Measure a target and report what was measured."""
