"""The control plane's domain model.

These types mirror the vocabulary in the Go data plane's ``internal/core``, but
they are not the wire format and not the database rows. Three separate shapes,
deliberately:

* **Domain** (here) — what the control plane reasons about. Refactored freely.
* **Wire** (``model_gateway_control.wire``) — generated, append-only, shared
  with the data plane.
* **Database** (``model_gateway_control.db``) — normalised for storage, with
  foreign keys and indexes that have nothing to do with either of the above.

Collapsing any two of them means every change to one becomes a compatibility
question about the others.
"""
