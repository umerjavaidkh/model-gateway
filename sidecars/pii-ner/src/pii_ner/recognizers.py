"""Recognisers, registered per language.

# On the default backend

The pattern backend shipped here is a placeholder for the statistical tier, and
is described as one rather than dressed up. It finds titled names, a small
gazetteer of organisation suffixes, and location prepositional phrases. That is
useful for demonstrating the boundary and genuinely catches some entities, and
it is not NER.

The value being delivered in this module is the *shape*: a registry keyed by
language, a recogniser protocol with one method, and a sidecar boundary the
gateway already speaks to. Swapping in Presidio or a transformer model changes
this file and nothing else — not the protocol, not the client, not the gateway.

Saying so plainly matters, because a pattern list presented as NER is exactly
the kind of control an operator believes is protecting them when it is not.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Protocol


@dataclass(frozen=True, slots=True)
class Entity:
    """One detected entity and where it was found.

    Offsets are byte offsets into the UTF-8 payload, not character indices. The
    gateway slices bytes, and a character index would be wrong for any text
    outside ASCII — which is most of the text this exists to handle.
    """

    kind: str
    start: int
    end: int
    value: str
    #: 0 to 1. The gateway does not currently threshold on it, but recording it
    #: means a future threshold does not need a protocol change.
    score: float = 0.5


class Recognizer(Protocol):
    """Finds entities of one or more kinds in one language."""

    @property
    def language(self) -> str:
        """The language tag this recogniser is written for, e.g. "en"."""

    def recognize(self, text: str) -> list[Entity]:
        """Return entities found in text, at byte offsets into its UTF-8 form."""


@dataclass(frozen=True, slots=True)
class PatternRecognizer:
    """A recogniser built from regular expressions.

    Byte offsets are computed by encoding the prefix, because a match index is
    a character index and the gateway works in bytes.
    """

    language: str
    patterns: tuple[tuple[str, re.Pattern[str], float], ...]

    def recognize(self, text: str) -> list[Entity]:
        found: list[Entity] = []
        for kind, pattern, score in self.patterns:
            for match in pattern.finditer(text):
                value = match.group(match.lastindex or 0)
                # The group may sit inside the match, so the offset is found
                # from the group's own span rather than the match's.
                start_char = match.start(match.lastindex or 0)
                start = len(text[:start_char].encode())
                found.append(
                    Entity(
                        kind=kind,
                        start=start,
                        end=start + len(value.encode()),
                        value=value,
                        score=score,
                    )
                )
        return found


def _english() -> PatternRecognizer:
    return PatternRecognizer(
        language="en",
        patterns=(
            # A title followed by capitalised words. Narrow on purpose: bare
            # capitalised words are sentence starts, product names and half of
            # ordinary prose, and flagging those would make redaction destroy
            # the text it was meant to protect.
            (
                "PERSON",
                re.compile(r"\b(?:Mr|Mrs|Ms|Dr|Prof)\.?\s+([A-Z][a-z]+(?:\s+[A-Z][a-z]+){0,2})"),
                0.6,
            ),
            (
                "ORGANIZATION",
                re.compile(
                    r"\b([A-Z][A-Za-z&.\-]*(?:\s+[A-Z][A-Za-z&.\-]*)*\s+"
                    r"(?:Inc|Ltd|LLC|GmbH|PLC|Corp|Corporation|Company)\.?)"
                ),
                0.7,
            ),
            (
                "LOCATION",
                re.compile(
                    r"\b(?:in|at|from|to)\s+([A-Z][a-z]+(?:\s+[A-Z][a-z]+)?)"
                    r"(?=[\s,.]|$)"
                ),
                0.4,
            ),
        ),
    )


def _arabic() -> PatternRecognizer:
    return PatternRecognizer(
        language="ar",
        patterns=(
            # Arabic honorifics followed by a name. The reason this file is
            # keyed by language at all: an English recogniser finds none of
            # these, and finds them silently — no error, just nothing.
            (
                "PERSON",
                re.compile(
                    r"(?:السيد|السيدة|الدكتور|الدكتورة|الأستاذ)\s+"
                    r"([ء-ي]+(?:\s+[ء-ي]+){0,2})"
                ),
                0.6,
            ),
            (
                "ORGANIZATION",
                re.compile(
                    r"((?:شركة|مؤسسة|مجموعة)\s+[ء-ي]+"
                    r"(?:\s+[ء-ي]+){0,2})"
                ),
                0.6,
            ),
        ),
    )


class Registry:
    """Recognisers, keyed by language.

    An unknown language falls back to the default rather than returning
    nothing, because returning nothing is indistinguishable from finding
    nothing — and the caller would treat "we have no recogniser for Urdu" as
    "this text is clean".
    """

    def __init__(self, default_language: str = "en") -> None:
        self._default = default_language
        self._by_language: dict[str, list[Recognizer]] = {}
        self.register(_english())
        self.register(_arabic())

    def register(self, recognizer: Recognizer) -> None:
        self._by_language.setdefault(recognizer.language, []).append(recognizer)

    def languages(self) -> list[str]:
        return sorted(self._by_language)

    def recognize(self, text: str, language: str | None = None) -> list[Entity]:
        """Run the recognisers for a language, or every one when none is named.

        Running all of them when the language is unspecified is the safe
        reading: a mixed-language payload is common, and the cost of an extra
        pattern pass is nothing next to the cost of missing an entity.
        """
        if language is None:
            recognizers = [r for group in self._by_language.values() for r in group]
        else:
            recognizers = self._by_language.get(language) or self._by_language[self._default]

        found: list[Entity] = []
        for recognizer in recognizers:
            found.extend(recognizer.recognize(text))
        return _deduplicate(found)


def _deduplicate(entities: list[Entity]) -> list[Entity]:
    """Drop overlapping entities, keeping the earliest and longest.

    Two recognisers finding the same span is normal when several languages run.
    Emitting both would make the gateway replace one and then find the other
    inside the placeholder it just wrote.
    """
    ordered = sorted(entities, key=lambda e: (e.start, -(e.end - e.start)))

    kept: list[Entity] = []
    end = -1
    for entity in ordered:
        if entity.start < end:
            continue
        kept.append(entity)
        end = entity.end
    return kept
