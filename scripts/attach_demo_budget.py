"""Attach the demo budget to the demo key, for the live check.

A separate file rather than a heredoc inside the shell script: nesting one
heredoc in another is how that script stopped parsing, and a Python program
embedded in a string is a Python program nobody can lint.
"""

from __future__ import annotations

import asyncio
import os

from model_gateway_control.db import models
from model_gateway_control.db.session import create_engine, session_factory


async def main() -> None:
    engine = create_engine(os.environ["GATEWAY_DATABASE_URL"])
    try:
        async with session_factory(engine)() as session:
            session.add(models.KeyBudget(key_id="budgeted-1", budget_id="demo-budget"))
            tenant = await session.get(models.Tenant, "demo")
            if tenant is not None:
                # The layer version must move, or the worker rejects the
                # snapshot carrying the new attachment.
                tenant.version += 1
            await session.commit()
    finally:
        await engine.dispose()


if __name__ == "__main__":
    asyncio.run(main())
