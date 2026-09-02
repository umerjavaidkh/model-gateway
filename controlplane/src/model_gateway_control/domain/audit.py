"""The audit chain: what makes a deleted record detectable.

Each record carries the hash of the one before it, so the chain is only
reproducible if every link is present and unaltered. Remove a row and every
hash after it stops matching; edit a row and its own hash stops matching. That
is the whole of the guarantee, and it is worth being exact about its limits:

**It detects tampering, it does not prevent it.** Anyone who can write the
table can rewrite the chain from the point they changed onward. What they
cannot do is change one row and leave the rest consistent, so the property is
"a change is visible to anyone who checks" rather than "a change is impossible".
Preventing it needs the head published somewhere the same person cannot write —
a notary, another organisation's storage, a printout. That is deployment
policy; this module gives it the value to publish.

**It says nothing about records that were never written.** A gap between what
happened and what reached the table is invisible to a chain over the table. The
stream's at-least-once delivery and the consumer's idempotency are what cover
that, not this.

The hash covers the record's meaning, not its storage: the sequence number, the
identity of the actor and the action, the outcome, and the previous hash. It
does not cover the row's insertion time, which the database chooses and which
would make the chain unverifiable after a restore.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from datetime import datetime

#: The hash of the record before the first one. A constant rather than an empty
#: string so that "the chain starts here" and "the previous hash was lost" are
#: different values.
GENESIS = "0" * 64

#: Separates fields inside the hashed payload. A byte that cannot appear in any
#: of the fields, so that no combination of values can be rearranged into the
#: same payload — the classic failure where ("ab", "c") and ("a", "bc") hash
#: alike.
_SEPARATOR = b"\x1f"

#: Prefixes the payload so a hash computed here cannot be replayed as a hash of
#: anything else this system signs.
_DOMAIN = b"model-gateway/audit/v1"


@dataclass(frozen=True, slots=True, kw_only=True)
class Link:
    """The fields of one record that the chain commits to."""

    seq: int
    event_id: str
    request_id: str
    occurred_at: datetime
    tenant: str
    actor: str
    action: str
    resource: str
    outcome: str
    reason: str
    source_ip: str
    snapshot_version: int


def compute_hash(link: Link, prev_hash: str) -> str:
    """Hash one record against its predecessor.

    Every field is length-prefixed as well as separated, because a separator
    alone is only unambiguous while no field can contain it — and a reason or a
    resource is free text that one day will.
    """
    parts = [
        _DOMAIN,
        str(link.seq).encode(),
        prev_hash.encode(),
        link.event_id.encode(),
        link.request_id.encode(),
        # Microseconds, normalised to UTC: a chain that hashes a local-time
        # rendering stops verifying when the process moves to another region.
        str(int(link.occurred_at.timestamp() * 1_000_000)).encode(),
        link.tenant.encode(),
        link.actor.encode(),
        link.action.encode(),
        link.resource.encode(),
        link.outcome.encode(),
        link.reason.encode(),
        link.source_ip.encode(),
        str(link.snapshot_version).encode(),
    ]

    digest = hashlib.sha256()
    for part in parts:
        digest.update(str(len(part)).encode())
        digest.update(_SEPARATOR)
        digest.update(part)
        digest.update(_SEPARATOR)
    return digest.hexdigest()
