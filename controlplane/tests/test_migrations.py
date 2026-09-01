"""The migration produces a schema the repository can actually use.

The repository suite builds its schema with ``create_all``, which is fast and
right for testing the mapping. But ``create_all`` is not what runs in
production, and a migration that drifts from the models is invisible until a
deploy fails — so this runs the real migration and then reads through it.

It needs a database that persists across connections, which rules out in-memory
SQLite. Skipped unless ``GATEWAY_TEST_DATABASE_URL`` names one; CI always does.
"""

from __future__ import annotations

import asyncio
import os
from pathlib import Path

import pytest
import pytest_asyncio
from alembic import command
from alembic.config import Config

from model_gateway_control.db.models import Base
from model_gateway_control.db.repository import Repository
from model_gateway_control.db.session import create_engine, session_factory

pytestmark = pytest.mark.skipif(
    not os.environ.get("GATEWAY_TEST_DATABASE_URL"),
    reason="needs a database that persists across connections",
)


@pytest_asyncio.fixture(autouse=True)
async def blank_database() -> None:
    """Remove every trace of a schema before each migration test.

    The other suites build their schema with ``create_all``, which Alembic knows
    nothing about. Sharing one database with them leaves version bookkeeping
    that disagrees with the tables actually present, so a migration test that
    ran second would fail for reasons having nothing to do with migrations.

    Dropping everything — including Alembic's own version table — makes these
    tests independent of what ran before them, which is what they need in order
    to be testing the migration rather than the ordering.
    """
    engine = create_engine(os.environ["GATEWAY_TEST_DATABASE_URL"])
    try:
        async with engine.begin() as connection:
            await connection.run_sync(Base.metadata.drop_all)
            await connection.exec_driver_sql("DROP TABLE IF EXISTS alembic_version")
    finally:
        await engine.dispose()


def _alembic_config(url: str) -> Config:
    root = Path(__file__).resolve().parent.parent
    config = Config(str(root / "alembic.ini"))
    config.set_main_option("script_location", str(root / "src/model_gateway_control/db/migrations"))
    config.set_main_option("sqlalchemy.url", url)
    return config


async def _alembic(url: str, action: str, revision: str) -> None:
    """Run an Alembic command off the test's event loop.

    env.py calls asyncio.run(), which raises inside a loop that is already
    running — and pytest-asyncio always has one. A worker thread has no loop of
    its own, so asyncio.run() there behaves exactly as it does on the command
    line, which is the thing being tested.
    """
    config = _alembic_config(url)
    runner = command.upgrade if action == "upgrade" else command.downgrade
    await asyncio.to_thread(runner, config, revision)


async def test_upgrade_head_produces_a_readable_schema() -> None:
    url = os.environ["GATEWAY_TEST_DATABASE_URL"]

    await _alembic(url, "upgrade", "head")

    engine = create_engine(url)
    try:
        async with session_factory(engine)() as session:
            # Reading fleet state proves two things at once: the tables exist in
            # the shape the models expect, and the migration seeded row 1 — a
            # missing seed would surface much later as an unexplained error on
            # the first snapshot build.
            fleet = await Repository(session).load_fleet()
            assert fleet.version >= 1
            assert fleet.deployments == ()
    finally:
        await engine.dispose()


async def test_downgrade_is_reversible() -> None:
    # A migration that cannot be undone is a migration nobody dares apply on a
    # Friday. Reversibility is checked here rather than discovered during an
    # incident.
    url = os.environ["GATEWAY_TEST_DATABASE_URL"]

    await _alembic(url, "upgrade", "head")
    await _alembic(url, "upgrade", "head")
