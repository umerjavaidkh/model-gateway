"""The migration produces a schema the repository can actually use.

The repository suite builds its schema with ``create_all``, which is fast and
right for testing the mapping. But ``create_all`` is not what runs in
production, and a migration that drifts from the models is invisible until a
deploy fails — so this runs the real migration and then reads through it.

It needs a database that persists across connections, which rules out in-memory
SQLite. Skipped unless ``GATEWAY_TEST_DATABASE_URL`` names one; CI always does.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest
from alembic import command
from alembic.config import Config

from model_gateway_control.db.repository import Repository
from model_gateway_control.db.session import create_engine, session_factory

pytestmark = pytest.mark.skipif(
    not os.environ.get("GATEWAY_TEST_DATABASE_URL"),
    reason="needs a database that persists across connections",
)


def _alembic_config(url: str) -> Config:
    root = Path(__file__).resolve().parent.parent
    config = Config(str(root / "alembic.ini"))
    config.set_main_option("script_location", str(root / "src/model_gateway_control/db/migrations"))
    config.set_main_option("sqlalchemy.url", url)
    return config


async def test_upgrade_head_produces_a_readable_schema() -> None:
    url = os.environ["GATEWAY_TEST_DATABASE_URL"]

    config = _alembic_config(url)
    command.downgrade(config, "base")
    command.upgrade(config, "head")

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
    config = _alembic_config(url)

    command.upgrade(config, "head")
    command.downgrade(config, "base")
    command.upgrade(config, "head")
