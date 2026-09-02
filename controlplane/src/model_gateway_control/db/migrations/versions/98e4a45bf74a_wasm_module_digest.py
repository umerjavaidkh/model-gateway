"""wasm module digest

Revision ID: 98e4a45bf74a
Revises: 9585f3d34653
Create Date: 2026-09-02 14:54:48.755637

"""

from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "98e4a45bf74a"
down_revision: Union[str, Sequence[str], None] = "9585f3d34653"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    """Upgrade schema."""
    # A server default, because the generated version has none and adding a
    # NOT NULL column without one fails on any table that already has rows.
    # Empty is right for every existing component: only in-process ones name a
    # module, and none existed before this migration.
    op.add_column(
        "components",
        sa.Column("module", sa.String(length=128), nullable=False, server_default=""),
    )


def downgrade() -> None:
    """Downgrade schema."""
    op.drop_column("components", "module")
