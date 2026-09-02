"""Run the fine-tune reconciler.

A separate process from the admin API, for the same reason accounting is: it is
a long-running loop with different failure characteristics. The API being down
blocks configuration changes; this being down only means jobs stop advancing —
training already in flight keeps running, and the next pass picks up where this
one stopped.

Nothing here decides anything. The loop's whole job is to call reconcile_once
on an interval and keep doing so; every decision about what a job may do next
lives in the domain, where it can be tested without a clock.
"""

from __future__ import annotations

import asyncio
import logging
import os
import signal

from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker

from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.errors import InvalidRequestError
from model_gateway_control.service.finetune import Evaluators, Reconciler, Trainers
from model_gateway_control.service.rollout import Policy as RolloutPolicy

logger = logging.getLogger("finetune")

#: How long to wait between passes.
#:
#: Training runs take hours, so polling faster buys nothing and costs a request
#: to every backend holding a job. A job that finishes just after a pass waits
#: at most this long to be noticed, which against a multi-hour run is noise.
DEFAULT_INTERVAL_SECONDS = 30.0


async def run(
    sessions: async_sessionmaker[AsyncSession],
    trainers: Trainers,
    evaluators: Evaluators | None = None,
    health: RolloutPolicy | None = None,
    interval: float = DEFAULT_INTERVAL_SECONDS,
    *,
    stop: asyncio.Event | None = None,
) -> None:
    """Reconcile until stopped."""
    stop = stop or asyncio.Event()
    reconciler = Reconciler(sessions, trainers, evaluators or Evaluators(), health=health)

    while not stop.is_set():
        try:
            for outcome in await reconciler.reconcile_once():
                if outcome.advanced:
                    logger.info("job %s is now %s", outcome.job.ref, outcome.job.status.phase)
            # A separate pass, because it answers a different question about a
            # different set of jobs: the first moves jobs towards an artifact,
            # this moves artifacts into production. Folding them together would
            # mean a training backlog delayed a rollout decision, and a stuck
            # rollout looked like a training problem.
            for outcome in await reconciler.advance_rollouts():
                logger.info(
                    "rollout for %s is at %d%%",
                    outcome.job.ref,
                    outcome.job.status.rollout_weight,
                )
        except Exception:
            # A pass that fails must not end the loop. Individual jobs already
            # survive a failing trainer; this catches the rest — a database
            # blip, a bug — because a reconciler that exits on the first
            # surprise stops advancing every job rather than one.
            logger.exception("a reconcile pass failed")

        with_timeout = asyncio.create_task(stop.wait())
        done, _ = await asyncio.wait({with_timeout}, timeout=interval)
        if not done:
            with_timeout.cancel()


def main() -> int:
    """Start the reconciler. Returns a process exit code."""
    logging.basicConfig(level=logging.INFO)

    database_url = os.environ.get("GATEWAY_DATABASE_URL", "")
    if not database_url:
        raise InvalidRequestError("GATEWAY_DATABASE_URL is required")

    interval = float(os.environ.get("GATEWAY_FINETUNE_INTERVAL", DEFAULT_INTERVAL_SECONDS))

    # Off unless asked for. A deployment that wants an operator to walk every
    # canary step gets exactly that, rather than an automation it has to
    # remember to turn off — and automatic promotion rests on an operational
    # signal, not a quality one, which a deployment may reasonably not want to
    # promote on. See service/rollout.py.
    health: RolloutPolicy | None = None
    if os.environ.get("GATEWAY_ROLLOUT_AUTOMATIC") == "true":
        health = RolloutPolicy()
        logger.info("canary steps will advance automatically on observed error rates")

    # No adapters are registered here. A deployment builds this process with
    # the ones it actually has; shipping one for a backend nobody can reach
    # from this repository would be code nothing has ever run, and the contract
    # suites are what such an adapter gets checked against wherever it lives.
    trainers = Trainers()
    evaluators = Evaluators()
    logger.warning("no trainer or eval adapters are registered; jobs will never advance")

    engine = create_engine(database_url)
    sessions = session_factory(engine)

    async def serve() -> None:
        stop = asyncio.Event()
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.add_signal_handler(sig, stop.set)
        try:
            await run(sessions, trainers, evaluators, health, interval, stop=stop)
        finally:
            await engine.dispose()

    asyncio.run(serve())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
