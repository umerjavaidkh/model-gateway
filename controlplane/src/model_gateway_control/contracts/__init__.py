"""Contract suites for control-plane ports.

The data plane has these for its four ports; the control plane's ports need
them for the same reason. An adapter that satisfies the type signature and not
the contract is the failure these catch, and for a TrainerPort that failure
costs a GPU bill rather than a wrong answer.
"""

from model_gateway_control.contracts.trainer import run_trainer_suite

__all__ = ["run_trainer_suite"]
