"""Generated protobuf bindings for the snapshot wire format.

The schema lives in ``proto/gateway/v1/snapshot.proto`` at the repository root
and is generated for both languages, so the Go data plane and this control
plane cannot drift apart. Regenerate with ``make proto``; CI fails if the
committed output no longer matches the schema.

Import the module directly::

    from model_gateway_control.wire import snapshot_pb2 as pb

There is deliberately no re-export here. A submodule is importable without one,
and writing ``from model_gateway_control.wire import snapshot_pb2`` inside this
file means the package importing itself.

Nothing under this package is hand-edited.
"""
