"""The HTTP surface, tested as the contract the Go client actually parses.

The client verifies every reported span against the UTF-8 bytes of the payload
and drops the ones that do not match, so a regression here does not corrupt a
request — it silently stops protecting it. That makes these assertions the
place the two languages agree about offsets.
"""

from fastapi.testclient import TestClient

from pii_ner.app import MAX_TEXT_BYTES, DetectResponse, create_app


def detect(client: TestClient, text: str, language: str | None = None) -> DetectResponse:
    """Call the sidecar and validate the reply against its own declared shape."""
    body: dict[str, object] = {"text": text}
    if language is not None:
        body["language"] = language
    response = client.post("/v1/detect", json=body)
    assert response.status_code == 200, response.text
    return DetectResponse.model_validate(response.json())


def test_offsets_are_bytes_not_characters() -> None:
    # An emoji before the name puts character and byte offsets four apart. The
    # Go side indexes bytes, so reporting characters would substitute over the
    # wrong span on every payload containing a single non-ASCII character.
    text = "🙂 email Dr. Ada Lovelace now"
    encoded = text.encode()

    with TestClient(create_app()) as client:
        entities = detect(client, text, "en").entities

    assert entities, "the recogniser found nothing to check offsets against"
    for entity in entities:
        assert encoded[entity.start : entity.end].decode() == entity.value


def test_oversized_text_is_truncated_rather_than_refused() -> None:
    # Refusing would make the gateway's fail-closed path reject a large
    # classified request outright; truncating inspects what fits and says so.
    with TestClient(create_app()) as client:
        payload = detect(client, "a" * (MAX_TEXT_BYTES + 100))

    assert payload.truncated is True


def test_ordinary_text_is_not_truncated() -> None:
    with TestClient(create_app()) as client:
        payload = detect(client, "nothing interesting here")

    assert payload.truncated is False
    assert payload.backend == "patterns"


def test_health_reports_the_backend_and_languages() -> None:
    # A sidecar answering with no recognisers loaded is worse than one that is
    # down, because the gateway treats a successful call as a clean scan.
    with TestClient(create_app()) as client:
        response = client.get("/healthz")

    assert response.status_code == 200
    payload = response.json()
    assert payload["status"] == "ok"
    assert payload["languages"]


def test_a_missing_text_field_is_rejected() -> None:
    with TestClient(create_app()) as client:
        assert client.post("/v1/detect", json={}).status_code == 422
