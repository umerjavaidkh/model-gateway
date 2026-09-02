"""Reading timestamps back out of the database.

One rule, in one place, because it has to be applied at every read: columns are
declared timezone-aware but not every driver honours that, and a naive value
that reaches arithmetic is either a ``TypeError`` or — worse — a silent offset.
"""

from __future__ import annotations

from datetime import UTC, datetime


def as_utc(when: datetime) -> datetime:
    """Attach UTC to a naive timestamp read from the database.

    SQLite has no timezone type and returns naive values; Postgres with
    ``timestamptz`` does not. Everything written is UTC, so a naive value read
    back *is* UTC — saying so here keeps the ambiguity from reaching code that
    would otherwise have to guess, or crash comparing it against an aware
    ``now``.
    """
    if when.tzinfo is None:
        return when.replace(tzinfo=UTC)
    return when
