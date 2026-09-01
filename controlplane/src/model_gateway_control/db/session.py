"""Engine and session construction.

The engine is built here and passed in everywhere else. Nothing reaches for a
module-level session: a component that constructs its own connection cannot be
tested without a database, and patching a module global is the workaround for a
dependency that should have been an argument.
"""

from __future__ import annotations

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

from sqlalchemy.ext.asyncio import (
    AsyncEngine,
    AsyncSession,
    async_sessionmaker,
    create_async_engine,
)

from model_gateway_control.db.models import Base


def create_engine(database_url: str, *, echo: bool = False) -> AsyncEngine:
    """Build an async engine for the given URL."""
    return create_async_engine(database_url, echo=echo, pool_pre_ping=True)


def session_factory(engine: AsyncEngine) -> async_sessionmaker[AsyncSession]:
    """Build a session factory.

    ``expire_on_commit=False`` because the caller reads attributes off returned
    objects after the session closes, and the default would turn every one of
    those reads into a lazy load against a dead connection.
    """
    return async_sessionmaker(engine, expire_on_commit=False)


@asynccontextmanager
async def session_scope(factory: async_sessionmaker[AsyncSession]) -> AsyncIterator[AsyncSession]:
    """Run a unit of work, committing on success and rolling back on failure."""
    async with factory() as session:
        try:
            yield session
            await session.commit()
        except Exception:
            await session.rollback()
            raise


async def create_all(engine: AsyncEngine) -> None:
    """Create every table.

    For tests and for a first local run. Real deployments migrate with Alembic,
    because ``create_all`` cannot express a change to an existing table and
    silently does nothing when one already exists in the wrong shape.
    """
    async with engine.begin() as connection:
        await connection.run_sync(Base.metadata.create_all)
