"""shadow traffic: usage attribution and the shadow sample rate

Revision ID: 44eb64162c2b
Revises: f349004e9362
Create Date: 2026-09-02 19:00:49.704398

"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = '44eb64162c2b'
down_revision: Union[str, Sequence[str], None] = 'f349004e9362'
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    """Upgrade schema."""
    op.add_column(
        'usage_records',
        sa.Column('deployment', sa.String(length=128), server_default='', nullable=False),
    )
    # sa.false() rather than the generated sa.text('0'). Autogenerate rendered
    # this against SQLite, where 0 is a boolean; Postgres refuses to default a
    # boolean column with an integer, and the migration test against a real
    # Postgres is what caught it.
    op.add_column(
        'usage_records',
        sa.Column('shadow', sa.Boolean(), server_default=sa.false(), nullable=False),
    )
    op.create_index(
        op.f('ix_usage_records_deployment'), 'usage_records', ['deployment'], unique=False
    )
    op.add_column(
        'finetune_jobs',
        sa.Column('shadow_percent', sa.Integer(), server_default='10', nullable=False),
    )


def downgrade() -> None:
    """Downgrade schema."""
    op.drop_column('finetune_jobs', 'shadow_percent')
    op.drop_index(op.f('ix_usage_records_deployment'), table_name='usage_records')
    op.drop_column('usage_records', 'shadow')
    op.drop_column('usage_records', 'deployment')
