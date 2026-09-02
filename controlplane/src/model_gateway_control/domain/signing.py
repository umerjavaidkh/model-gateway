"""Publisher signatures over component manifests.

The registry already binds an admission record to a manifest digest, so a
component that was edited after it passed no longer matches what passed. That
answers "are these the bytes that were tested". It does not answer "who
submitted them", and a registry where anyone who can reach the admin API can
publish a component under any name is one where the answer to "where did this
come from" is "the database says so".

A signature answers it with something the database cannot manufacture.

# What this protects against, and what it does not

The trust root is configuration, not a table: keys are loaded from a file the
control plane reads at startup. That is the entire point. An attacker who owns
the database can set a component's status to active and can write whatever they
like into its signature columns — but the stored signature is evidence to be
re-checked, not a claim to be believed, and re-checking uses keys they did not
get with the database.

So the signature is verified twice: once at registration, where a bad one gets
a clear error while a publisher is watching, and again when a snapshot binds
the component, which is the check that actually gates production. The second is
the one that matters. Without it, "verified at registration" is a fact about a
row, and rows are what an attacker with a database has.

It protects against nothing at all if the trusted-keys file is writable by
whatever compromised the database.
"""

from __future__ import annotations

import base64
import binascii
import json
from dataclasses import dataclass
from enum import StrEnum
from pathlib import Path

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import (
    Ed25519PrivateKey,
    Ed25519PublicKey,
)

from model_gateway_control.errors import ForbiddenError, InvalidRequestError

#: Prefixed to the digest before signing.
#:
#: Ed25519 signs whatever bytes it is given, so a signature over a bare hash is
#: valid in any other protocol that signs bare hashes with the same key. The
#: prefix says what this signature is for, and makes a signature issued here
#: useless anywhere else.
SIGNING_CONTEXT = b"model-gateway/manifest/v1:"

#: Ed25519 public keys are 32 bytes and signatures are 64. Checked explicitly
#: so a truncated value fails with something an operator can read rather than
#: inside the library.
_PUBLIC_KEY_BYTES = 32
_SIGNATURE_BYTES = 64


class KeyStatus(StrEnum):
    """Whether a key may sign, and whether what it signed still counts."""

    #: May sign new registrations.
    ACTIVE = "active"
    #: May not sign new registrations; what it already signed stays valid.
    #: This is planned rotation — the key is being replaced, not distrusted.
    RETIRED = "retired"
    #: Distrusted entirely. Nothing it signed verifies anywhere, so revoking a
    #: key takes its components out of the next snapshot. That is what
    #: revocation means and it is deliberately blunt: a key is revoked because
    #: it is believed compromised, and a compromised key's components are
    #: exactly what must stop running.
    REVOKED = "revoked"


class Policy(StrEnum):
    """How strictly signatures are enforced."""

    #: Signatures are verified when present and never required. The default,
    #: because turning this on before any publisher has a key would lock every
    #: existing component out of every future snapshot.
    OPTIONAL = "optional"
    #: Every component must carry a valid signature from a trusted key, both to
    #: register and to be bound by a snapshot.
    REQUIRED = "required"


@dataclass(frozen=True, slots=True)
class Signature:
    """A publisher's signature over a manifest digest."""

    #: Which key signed. Names the key rather than the publisher so that
    #: rotation does not need every manifest re-signed under a new identity.
    key_id: str
    #: The raw Ed25519 signature.
    value: bytes

    def __post_init__(self) -> None:
        if not self.key_id:
            raise InvalidRequestError("a signature must name the key that produced it")
        if len(self.value) != _SIGNATURE_BYTES:
            raise InvalidRequestError(
                f"signature from {self.key_id!r} is {len(self.value)} bytes, "
                f"not the {_SIGNATURE_BYTES} an Ed25519 signature is"
            )

    def encoded(self) -> str:
        """The signature as base64, which is how it crosses the API."""
        return base64.b64encode(self.value).decode()

    @classmethod
    def decode(cls, key_id: str, value: str) -> Signature:
        """Build a signature from the base64 an API request carries."""
        return cls(key_id=key_id, value=_decode_base64(value, f"signature from {key_id!r}"))


@dataclass(frozen=True, slots=True)
class PublisherKey:
    """A key the control plane will accept signatures from."""

    key_id: str
    #: Who holds it. Recorded for the audit trail: "which publisher submitted
    #: this" is the question a signature exists to answer.
    publisher: str
    public_key: bytes
    status: KeyStatus = KeyStatus.ACTIVE

    def __post_init__(self) -> None:
        if not self.key_id:
            raise InvalidRequestError("a publisher key needs an id")
        if not self.publisher:
            raise InvalidRequestError(f"key {self.key_id!r} needs a publisher")
        if len(self.public_key) != _PUBLIC_KEY_BYTES:
            raise InvalidRequestError(
                f"key {self.key_id!r} is {len(self.public_key)} bytes, "
                f"not the {_PUBLIC_KEY_BYTES} an Ed25519 public key is"
            )


class TrustStore:
    """The keys this control plane accepts, and the policy it applies.

    Immutable once built. It is loaded from configuration at startup precisely
    so that changing it takes the access that changing configuration takes,
    rather than the access that writing a row takes.
    """

    def __init__(self, keys: tuple[PublisherKey, ...] = (), policy: Policy = Policy.OPTIONAL):
        by_id: dict[str, PublisherKey] = {}
        for key in keys:
            if key.key_id in by_id:
                raise InvalidRequestError(f"two publisher keys share the id {key.key_id!r}")
            by_id[key.key_id] = key
        if policy is Policy.REQUIRED and not by_id:
            # Every registration and every snapshot would fail, which looks
            # exactly like the software being broken.
            raise InvalidRequestError(
                "signatures are required but no publisher keys are configured"
            )
        self._keys = by_id
        self._policy = policy

    @property
    def policy(self) -> Policy:
        """How strictly signatures are enforced."""
        return self._policy

    def key(self, key_id: str) -> PublisherKey | None:
        """The key with this id, if it is one this control plane knows."""
        return self._keys.get(key_id)

    def verify(self, digest: str, signature: Signature | None) -> PublisherKey | None:
        """Check a signature over a manifest digest.

        Returns the key that signed, or None when there is no signature and
        none is required. Raises otherwise — a signature that does not verify
        is never treated as an absent one, because "we could not check it" and
        "there was nothing to check" mean opposite things.
        """
        if signature is None:
            if self._policy is Policy.REQUIRED:
                raise ForbiddenError(
                    "this control plane requires a publisher signature on every component"
                )
            return None

        key = self._keys.get(signature.key_id)
        if key is None:
            raise ForbiddenError(
                f"signature names key {signature.key_id!r}, which is not a trusted publisher key"
            )
        if key.status is KeyStatus.REVOKED:
            raise ForbiddenError(
                f"key {key.key_id!r} ({key.publisher}) is revoked; nothing it signed is trusted"
            )

        try:
            Ed25519PublicKey.from_public_bytes(key.public_key).verify(
                signature.value, signing_payload(digest)
            )
        except InvalidSignature:
            raise ForbiddenError(
                f"the signature from {key.key_id!r} does not match this manifest"
            ) from None
        return key

    def verify_for_registration(
        self, digest: str, signature: Signature | None
    ) -> PublisherKey | None:
        """Check a signature that is being submitted now.

        Stricter than verify: a retired key may not sign anything new. What it
        signed before stays valid, which is what makes rotation possible
        without re-signing every manifest in the registry.
        """
        key = self.verify(digest, signature)
        if key is not None and key.status is KeyStatus.RETIRED:
            raise ForbiddenError(
                f"key {key.key_id!r} ({key.publisher}) is retired and cannot sign new components"
            )
        return key

    @classmethod
    def from_file(cls, path: str | Path, policy: Policy = Policy.OPTIONAL) -> TrustStore:
        """Load keys from the JSON file a deployment configures.

        A file rather than a table, and read at startup rather than per
        request: this is the one input that must not be changeable by whatever
        can change the database.
        """
        location = Path(path)
        try:
            raw = json.loads(location.read_text())
        except OSError as exc:
            raise InvalidRequestError(f"reading trusted keys from {location}: {exc}") from exc
        except json.JSONDecodeError as exc:
            raise InvalidRequestError(
                f"trusted keys in {location} are not valid JSON: {exc}"
            ) from exc

        return cls(keys=parse_keys(raw, str(location)), policy=policy)


def parse_keys(raw: object, where: str) -> tuple[PublisherKey, ...]:
    """Build publisher keys from the parsed trusted-keys document."""
    if not isinstance(raw, dict) or not isinstance(raw.get("keys"), list):
        raise InvalidRequestError(f"{where} must be an object with a 'keys' list")

    keys = []
    for index, entry in enumerate(raw["keys"]):
        if not isinstance(entry, dict):
            raise InvalidRequestError(f"{where}: key {index} is not an object")
        try:
            status = KeyStatus(str(entry.get("status", KeyStatus.ACTIVE)))
        except ValueError as exc:
            raise InvalidRequestError(f"{where}: key {index} has an unknown status") from exc
        keys.append(
            PublisherKey(
                key_id=str(entry.get("key_id", "")),
                publisher=str(entry.get("publisher", "")),
                public_key=_decode_base64(
                    str(entry.get("public_key", "")), f"{where}: key {index}"
                ),
                status=status,
            )
        )
    return tuple(keys)


def signing_payload(digest: str) -> bytes:
    """The exact bytes a publisher signs.

    One function, used by the signer and the verifier, so the two cannot
    disagree about what was covered.
    """
    if not digest:
        raise InvalidRequestError("there is nothing to sign without a manifest digest")
    return SIGNING_CONTEXT + digest.encode()


def sign(digest: str, private_key: Ed25519PrivateKey, key_id: str) -> Signature:
    """Sign a manifest digest. Used by the publisher tooling."""
    return Signature(key_id=key_id, value=private_key.sign(signing_payload(digest)))


def _decode_base64(value: str, where: str) -> bytes:
    if not value:
        raise InvalidRequestError(f"{where} is empty")
    try:
        # validate=True so that whitespace or stray characters are an error
        # rather than being silently dropped into a different key.
        return base64.b64decode(value, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise InvalidRequestError(f"{where} is not valid base64") from exc
