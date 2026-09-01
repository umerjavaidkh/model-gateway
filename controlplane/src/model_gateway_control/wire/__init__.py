"""Generated protobuf bindings for the snapshot wire format.

The schema lives in ``proto/gateway/v1/snapshot.proto`` at the repository root
and is generated for both languages, so the Go data plane and this control
plane cannot drift apart. Regenerate with ``make proto``; CI fails if the
committed output no longer matches the schema.

Nothing here is hand-edited.
"""

from model_gateway_control.wire import snapshot_pb2

__all__ = ["snapshot_pb2"]
