"""richer usage records

Revision ID: 645415bffe99
Revises: c30ded1a4302
Create Date: 2026-09-02 23:01:47.186094

"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = '645415bffe99'
down_revision: Union[str, Sequence[str], None] = 'c30ded1a4302'
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    """Upgrade schema."""
    op.add_column('usage_records', sa.Column('base_model', sa.String(length=255), server_default=sa.text("('')"), nullable=False))
    op.add_column('usage_records', sa.Column('adapter_id', sa.String(length=255), server_default=sa.text("('')"), nullable=False))
    op.add_column('usage_records', sa.Column('provider', sa.String(length=64), server_default=sa.text("('')"), nullable=False))
    op.add_column('usage_records', sa.Column('stream', sa.Boolean(), server_default=sa.false(), nullable=False))
    op.add_column('usage_records', sa.Column('latency_ms', sa.Integer(), server_default=sa.text('0'), nullable=False))
    op.add_column('usage_records', sa.Column('time_to_first_byte_ms', sa.Integer(), server_default=sa.text('0'), nullable=False))
    op.add_column('usage_records', sa.Column('snapshot_version', sa.BigInteger(), server_default=sa.text('0'), nullable=False))
    op.add_column('usage_records', sa.Column('stages', sa.Text(), server_default=sa.text("'[]'"), nullable=False))


def downgrade() -> None:
    """Downgrade schema."""
    op.drop_column('usage_records', 'stages')
    op.drop_column('usage_records', 'snapshot_version')
    op.drop_column('usage_records', 'time_to_first_byte_ms')
    op.drop_column('usage_records', 'latency_ms')
    op.drop_column('usage_records', 'stream')
    op.drop_column('usage_records', 'provider')
    op.drop_column('usage_records', 'adapter_id')
    op.drop_column('usage_records', 'base_model')
