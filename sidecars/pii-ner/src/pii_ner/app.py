"""The sidecar's HTTP surface.

JSON rather than protobuf, deliberately. The gateway's snapshot is protobuf
because it crosses a language boundary *and* a version boundary — an old worker
must read a new snapshot. This crosses only a language boundary, between two
processes deployed together in one pod at one version, and the message is two
fields. JSON costs nothing here and can be inspected with curl over the socket
when something is wrong at three in the morning.
"""

from __future__ import annotations

from typing import Annotated

from fastapi import FastAPI
from pydantic import BaseModel, Field

from pii_ner.recognizers import Registry

#: Bounds the work. A statistical pass is expensive and the gateway already
#: caps its own request bodies; a sidecar that scales without limit turns one
#: oversized request into a memory problem for the whole pod.
MAX_TEXT_BYTES = 256 * 1024


class DetectRequest(BaseModel):
    """Text to inspect."""

    text: str
    #: Omitted means "run every language". A mixed-language payload is common,
    #: and an extra pattern pass costs nothing next to missing an entity.
    language: str | None = None


class DetectedEntity(BaseModel):
    """One entity, at byte offsets into the UTF-8 text."""

    kind: str
    start: Annotated[int, Field(ge=0)]
    end: Annotated[int, Field(ge=0)]
    value: str
    score: float


class DetectResponse(BaseModel):
    """What was found."""

    entities: list[DetectedEntity]
    #: Reported so the gateway can record which tier answered, and so a
    #: misconfigured sidecar is visible rather than merely quiet.
    backend: str
    truncated: bool = False


def create_app(registry: Registry | None = None, backend: str = "patterns") -> FastAPI:
    """Build the sidecar application."""
    registry = registry or Registry()
    app = FastAPI(title="PII NER sidecar", version="0.1.0")

    @app.get("/healthz")
    async def healthz() -> dict[str, object]:
        return {"status": "ok", "backend": backend, "languages": registry.languages()}

    @app.post("/v1/detect")
    async def detect(request: DetectRequest) -> DetectResponse:
        encoded = request.text.encode()
        truncated = len(encoded) > MAX_TEXT_BYTES
        text = encoded[:MAX_TEXT_BYTES].decode(errors="ignore") if truncated else request.text

        entities = registry.recognize(text, request.language)
        return DetectResponse(
            entities=[
                DetectedEntity(kind=e.kind, start=e.start, end=e.end, value=e.value, score=e.score)
                for e in entities
            ],
            backend=backend,
            truncated=truncated,
        )

    return app
