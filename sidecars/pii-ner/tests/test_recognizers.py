"""The recogniser registry.

These assert the *shape* the sidecar guarantees, not the quality of the
placeholder backend: byte offsets, per-language dispatch, and overlap handling
are what the gateway depends on and what must survive swapping the backend for
a real model.
"""

from __future__ import annotations

import itertools

from pii_ner.recognizers import Registry


def test_offsets_are_bytes_not_characters() -> None:
    # The gateway slices bytes. A character index is wrong for any text outside
    # ASCII — which is most of the text this exists to handle — and the error
    # would be a silently mangled replacement rather than a crash.
    registry = Registry()
    text = "مرحبا Dr. Ada Lovelace"

    entities = registry.recognize(text, language="en")
    assert entities, "no entity was found"

    entity = entities[0]
    assert text.encode()[entity.start : entity.end].decode() == entity.value


def test_an_english_recognizer_finds_nothing_in_arabic() -> None:
    # The reason the registry is keyed by language at all. An English model
    # missing Arabic entities is not a smaller version of the right answer, and
    # it fails silently: no error, just nothing.
    registry = Registry()
    arabic = "اجتمعت مع السيد أحمد الفلاسي أمس"

    assert registry.recognize(arabic, language="en") == []
    assert registry.recognize(arabic, language="ar"), "the Arabic recogniser found nothing"


def test_an_unknown_language_falls_back_rather_than_returning_nothing() -> None:
    # Returning nothing is indistinguishable from finding nothing, and the
    # caller would read "we have no recogniser for Urdu" as "this text is
    # clean".
    registry = Registry()
    entities = registry.recognize("Dr. Ada Lovelace called", language="ur")
    assert entities, "an unknown language returned nothing instead of falling back"


def test_no_language_runs_every_recognizer() -> None:
    registry = Registry()
    mixed = "Dr. Ada Lovelace met السيد أحمد الفلاسي"

    kinds = {e.value for e in registry.recognize(mixed)}
    assert any("Ada" in value for value in kinds)
    assert any("أحمد" in value for value in kinds)


def test_overlapping_entities_are_deduplicated() -> None:
    # Emitting both would make the gateway replace one and then find the other
    # inside the placeholder it just wrote.
    registry = Registry()
    entities = registry.recognize("Dr. Ada Lovelace works at Contoso Ltd")

    spans = sorted((e.start, e.end) for e in entities)
    for (_, first_end), (second_start, _) in itertools.pairwise(spans):
        assert second_start >= first_end, f"overlapping spans: {spans}"


def test_ordinary_prose_is_not_flagged() -> None:
    # A recogniser that fires on normal text makes redaction destroy the
    # content it was meant to protect, and then it gets turned off.
    registry = Registry()
    assert registry.recognize("please summarise the quarterly report") == []
