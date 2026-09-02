"""publisher signatures

Revision ID: 4ff34a50585d
Revises: 98e4a45bf74a
Create Date: 2026-09-02 16:29:05.411881

"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = '4ff34a50585d'
down_revision: Union[str, Sequence[str], None] = '98e4a45bf74a'
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    """Upgrade schema."""
    # Server defaults, because adding a NOT NULL column without one fails on
    # any table that already has rows. Empty is right for every existing
    # component: none was registered with a signature, and the policy check is
    # what decides whether unsigned is allowed.
    op.add_column(
        'components',
        sa.Column('signing_key_id', sa.String(length=128), nullable=False, server_default=''),
    )
    op.add_column(
        'components',
        sa.Column('signature', sa.String(length=256), nullable=False, server_default=''),
    )


def downgrade() -> None:
    """Downgrade schema."""
    op.drop_column('components', 'signature')
    op.drop_column('components', 'signing_key_id')

