"""Persistence for the control plane.

The database is the control plane's source of truth. It is *not* the domain
model and *not* the wire format: rows are normalised for storage, with foreign
keys and indexes that have nothing to do with how the builder reasons or how
the data plane reads. `repository.py` is the only place that knows both shapes.

The data plane never appears here. It holds no durable state and reads no
database, which is what lets a control-plane outage degrade to "configuration is
frozen" rather than "traffic stops".
"""
