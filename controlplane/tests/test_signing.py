"""Publisher signatures: what they prove, and where that is checked."""

from __future__ import annotations

import base64
import json
from pathlib import Path

import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat

from model_gateway_control.domain.signing import (
    SIGNING_CONTEXT,
    KeyStatus,
    Policy,
    PublisherKey,
    Signature,
    TrustStore,
    sign,
    signing_payload,
)
from model_gateway_control.errors import ForbiddenError, InvalidRequestError

DIGEST = "a" * 64
OTHER_DIGEST = "b" * 64


def keypair(
    key_id: str = "acme-2026", status: KeyStatus = KeyStatus.ACTIVE
) -> tuple[Ed25519PrivateKey, PublisherKey]:
    private = Ed25519PrivateKey.generate()
    public = private.public_key().public_bytes(encoding=Encoding.Raw, format=PublicFormat.Raw)
    return private, PublisherKey(key_id=key_id, publisher="ACME", public_key=public, status=status)


def test_a_valid_signature_names_the_publisher_that_produced_it() -> None:
    private, key = keypair()
    store = TrustStore(keys=(key,))

    signer = store.verify(DIGEST, sign(DIGEST, private, key.key_id))

    assert signer is not None
    assert signer.publisher == "ACME"


def test_a_signature_over_a_different_manifest_does_not_verify() -> None:
    # The whole point: a manifest edited after signing is a different digest,
    # so the signature stops covering it.
    private, key = keypair()
    store = TrustStore(keys=(key,))
    signature = sign(OTHER_DIGEST, private, key.key_id)

    with pytest.raises(ForbiddenError, match="does not match this manifest"):
        store.verify(DIGEST, signature)


def test_a_signature_from_an_untrusted_key_is_refused() -> None:
    # Anyone can generate a keypair. What makes one trusted is being in the
    # configured store, and nothing else.
    stranger, stranger_key = keypair(key_id="stranger")
    _, known = keypair()
    store = TrustStore(keys=(known,))

    with pytest.raises(ForbiddenError, match="not a trusted publisher key"):
        store.verify(DIGEST, sign(DIGEST, stranger, stranger_key.key_id))


def test_a_signature_that_claims_a_trusted_key_it_does_not_hold_is_refused() -> None:
    # The interesting forgery: the attacker knows a valid key id and signs with
    # their own key.
    stranger, _ = keypair()
    _, known = keypair()
    store = TrustStore(keys=(known,))

    with pytest.raises(ForbiddenError, match="does not match this manifest"):
        store.verify(DIGEST, sign(DIGEST, stranger, known.key_id))


def test_signatures_are_domain_separated() -> None:
    # Ed25519 signs whatever bytes it is given, so a signature over a bare hash
    # would be valid in any other protocol that signs bare hashes with the same
    # key. The prefix says what this signature is for.
    assert signing_payload(DIGEST).startswith(SIGNING_CONTEXT)

    private, key = keypair()
    bare = Signature(key_id=key.key_id, value=private.sign(DIGEST.encode()))
    store = TrustStore(keys=(key,))

    with pytest.raises(ForbiddenError):
        store.verify(DIGEST, bare)


# --- policy -----------------------------------------------------------------


def test_an_unsigned_component_is_allowed_only_when_signatures_are_optional() -> None:
    # Turning enforcement on before publishers have keys would lock every
    # existing component out of every future snapshot, so optional is default.
    _, key = keypair()

    assert TrustStore(keys=(key,)).verify(DIGEST, None) is None

    with pytest.raises(ForbiddenError, match="requires a publisher signature"):
        TrustStore(keys=(key,), policy=Policy.REQUIRED).verify(DIGEST, None)


def test_requiring_signatures_with_no_keys_is_refused_at_startup() -> None:
    # Every registration and every snapshot build would fail, which looks
    # exactly like the software being broken.
    with pytest.raises(InvalidRequestError, match="no publisher keys are configured"):
        TrustStore(policy=Policy.REQUIRED)


# --- rotation and revocation ------------------------------------------------


def test_a_retired_key_cannot_sign_new_work_but_its_old_work_stands() -> None:
    # Planned rotation. Invalidating everything a key ever signed would mean
    # re-signing the whole registry to replace one key.
    private, key = keypair(status=KeyStatus.RETIRED)
    store = TrustStore(keys=(key,))
    signature = sign(DIGEST, private, key.key_id)

    assert store.verify(DIGEST, signature) is not None

    with pytest.raises(ForbiddenError, match="retired"):
        store.verify_for_registration(DIGEST, signature)


def test_a_revoked_key_invalidates_everything_it_signed() -> None:
    # A key is revoked because it is believed compromised, and a compromised
    # key's components are exactly the ones that must stop running.
    private, key = keypair(status=KeyStatus.REVOKED)
    store = TrustStore(keys=(key,))
    signature = sign(DIGEST, private, key.key_id)

    for check in (store.verify, store.verify_for_registration):
        with pytest.raises(ForbiddenError, match="revoked"):
            check(DIGEST, signature)


# --- loading ----------------------------------------------------------------


def test_keys_load_from_the_configured_file(tmp_path: Path) -> None:
    _, key = keypair()
    path = tmp_path / "keys.json"
    path.write_text(
        json.dumps(
            {
                "keys": [
                    {
                        "key_id": key.key_id,
                        "publisher": key.publisher,
                        "public_key": base64.b64encode(key.public_key).decode(),
                        "status": "active",
                    }
                ]
            }
        )
    )

    store = TrustStore.from_file(path)

    loaded = store.key(key.key_id)
    assert loaded is not None
    assert loaded.public_key == key.public_key


def test_a_malformed_trust_store_fails_loudly(tmp_path: Path) -> None:
    # It is the security boundary. Falling back to "no keys" on a typo would
    # turn a required policy into an outage or, worse, a silent downgrade.
    for content, match in (
        ("not json at all", "not valid JSON"),
        (json.dumps({"keys": "acme"}), "'keys' list"),
        (
            json.dumps({"keys": [{"key_id": "a", "publisher": "b", "public_key": "!!!"}]}),
            "not valid base64",
        ),
        (
            json.dumps({"keys": [{"key_id": "a", "publisher": "b", "public_key": "YQ=="}]}),
            "not the 32",
        ),
        (
            json.dumps({"keys": [{"key_id": "", "publisher": "b", "public_key": "YQ=="}]}),
            "needs an id",
        ),
    ):
        path = tmp_path / "keys.json"
        path.write_text(content)
        with pytest.raises(InvalidRequestError, match=match):
            TrustStore.from_file(path)


def test_a_missing_trust_store_file_fails_loudly(tmp_path: Path) -> None:
    with pytest.raises(InvalidRequestError, match="reading trusted keys"):
        TrustStore.from_file(tmp_path / "absent.json")


def test_duplicate_key_ids_are_refused() -> None:
    # Which one verifies would depend on ordering, and revoking one would
    # silently leave the other working.
    _, first = keypair()
    _, second = keypair()

    with pytest.raises(InvalidRequestError, match="share the id"):
        TrustStore(keys=(first, second))


def test_a_truncated_signature_is_refused_before_verification() -> None:
    with pytest.raises(InvalidRequestError, match="not the 64"):
        Signature(key_id="acme", value=b"short")
    with pytest.raises(InvalidRequestError, match="must name the key"):
        Signature(key_id="", value=b"\x00" * 64)
    with pytest.raises(InvalidRequestError, match="not valid base64"):
        Signature.decode("acme", "!!!not base64!!!")
