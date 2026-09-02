"""The registry's invariants, which are all about what cannot be bound.

Every case here is a way a component could reach a snapshot without having
earned it. The registry is only worth having if each one is closed.
"""

from __future__ import annotations

import dataclasses

import pytest

from model_gateway_control.domain.component import (
    Admission,
    Component,
    Execution,
    FailureMode,
    Manifest,
    Port,
    Registry,
    Status,
    admitted,
    manifest_from_dict,
)
from model_gateway_control.errors import InvalidRequestError, NotFoundError


def manifest(**overrides: object) -> Manifest:
    defaults: dict[str, object] = {
        "name": "presidio",
        "version": "2.1.0",
        "port": Port.GUARDRAIL,
        "latency_budget_ms": 50,
    }
    return Manifest(**(defaults | overrides))  # type: ignore[arg-type]


# --- the manifest itself ----------------------------------------------------


def test_a_version_that_is_not_semver_is_rejected() -> None:
    # Versions identify artifacts and appear in bindings. "latest" identifies
    # whatever was published most recently, which is not an artifact.
    for bad in ("latest", "2.1", "v2.1.0", "2.1.0.0", ""):
        with pytest.raises(InvalidRequestError, match="semver"):
            manifest(version=bad)


def test_a_name_that_could_not_appear_in_a_metric_label_is_rejected() -> None:
    for bad in ("Presidio", "pii scan", "a", "pii_scan", "-pii"):
        with pytest.raises(InvalidRequestError, match="lowercase slug"):
            manifest(name=bad)


def test_a_request_path_component_must_declare_a_latency_budget() -> None:
    # Zero is not "fast", it is "unstated" — and an unstated budget is one no
    # binding can be checked against.
    with pytest.raises(InvalidRequestError, match="latency budget"):
        manifest(latency_budget_ms=0)


def test_a_control_plane_component_needs_no_latency_budget() -> None:
    # A training job takes hours. Demanding a millisecond budget would force
    # publishers to write a number that means nothing.
    trainer = manifest(name="llamafactory", port=Port.TRAINER, latency_budget_ms=0)
    assert trainer.latency_budget_ms == 0


def test_an_image_pinned_by_tag_is_rejected() -> None:
    # A floating tag turns the admitted artifact into a different one silently,
    # which defeats the gate entirely.
    for tagged in (
        "ghcr.io/acme/presidio:latest",
        "ghcr.io/acme/presidio",
        "sha256:abc123",  # truncated, so not a content address
        "@sha256:" + "a" * 64,  # no name before the digest
        "ghcr.io/acme/presidio@sha256:" + "z" * 64,  # not hex
    ):
        with pytest.raises(InvalidRequestError, match="image digest"):
            manifest(execution=Execution.SIDECAR, image=tagged)


def test_both_forms_of_digest_pin_are_accepted() -> None:
    # A repository digest is what a published component carries. A bare image
    # ID is what a locally built or air-gapped image has, and it is exactly as
    # immutable — accepting only the first would mean an air-gapped deployment
    # could admit nothing.
    for pinned in ("ghcr.io/acme/presidio@sha256:" + "a" * 64, "sha256:" + "b" * 64):
        assert manifest(image=pinned).image == pinned


def test_a_config_schema_that_is_not_a_json_object_is_rejected() -> None:
    with pytest.raises(InvalidRequestError, match="not JSON"):
        manifest(config_schema="{not json")
    with pytest.raises(InvalidRequestError, match="not an object"):
        manifest(config_schema="[]")


def test_the_digest_covers_every_field_and_is_order_independent() -> None:
    base = manifest(capabilities=("network", "streaming"))

    assert base.digest() == manifest(capabilities=("streaming", "network")).digest()
    for change in (
        {"version": "2.1.1"},
        {"latency_budget_ms": 51},
        {"failure_mode": FailureMode.OPEN},
        {"execution": Execution.IN_PROCESS},
        {"config_schema": '{"type":"object"}'},
        {"capabilities": ("streaming",)},
    ):
        assert base.digest() != manifest(**({"capabilities": base.capabilities} | change)).digest()


def test_an_unknown_manifest_field_is_an_error_rather_than_a_default() -> None:
    # A misspelled latency_budget_ms would otherwise be admitted with the
    # default, and the publisher would have no way to tell.
    with pytest.raises(InvalidRequestError, match="latency_budget"):
        manifest_from_dict(
            {"name": "presidio", "version": "2.1.0", "port": "guardrail", "latency_budget": 50}
        )


# --- admission --------------------------------------------------------------


def test_a_component_cannot_be_active_without_a_passing_admission() -> None:
    with pytest.raises(InvalidRequestError, match="passing admission"):
        Component(manifest=manifest(), status=Status.ACTIVE)


def test_a_failing_admission_does_not_activate() -> None:
    with pytest.raises(InvalidRequestError, match="passing admission"):
        Component(
            manifest=manifest(),
            status=Status.ACTIVE,
            admission=Admission(
                suite=Port.GUARDRAIL,
                suite_version="1",
                manifest_digest=manifest().digest(),
                passed=False,
                runner="sandbox",
            ),
        )


def test_editing_an_admitted_manifest_loses_its_admission() -> None:
    # The record binds to a digest rather than to a name and version, so an
    # edit does not inherit the verdict that covered the previous bytes. The
    # invariant is enforced in the constructor, so the edited component cannot
    # be built at all rather than being built and quietly unadmitted.
    original = admitted(manifest(), suite_version="1", runner="sandbox")
    assert original.is_admitted

    with pytest.raises(InvalidRequestError, match="passing admission"):
        dataclasses.replace(
            original, manifest=dataclasses.replace(original.manifest, latency_budget_ms=500)
        )


def test_an_admission_from_the_wrong_suite_does_not_admit() -> None:
    # Passing the provider battery says nothing about whether something is a
    # working guardrail.
    wrong = Component(
        manifest=manifest(),
        admission=Admission(
            suite=Port.PROVIDER,
            suite_version="1",
            manifest_digest=manifest().digest(),
            passed=True,
            runner="sandbox",
        ),
    )
    assert not wrong.is_admitted


def test_an_admission_must_name_what_ran_it() -> None:
    # "The control plane ran it in-process" has to be something an auditor can
    # rule out, which needs the runner on the record.
    with pytest.raises(InvalidRequestError, match="what ran the suite"):
        Admission(
            suite=Port.GUARDRAIL,
            suite_version="1",
            manifest_digest="0" * 64,
            passed=True,
            runner="",
        )


# --- resolution -------------------------------------------------------------


def test_resolving_an_unregistered_component_says_so() -> None:
    with pytest.raises(NotFoundError, match="presidio"):
        Registry().resolve(Port.GUARDRAIL, "presidio", "2.1.0")


def test_resolving_the_same_name_on_another_port_does_not_match() -> None:
    registry = Registry((admitted(manifest(), suite_version="1", runner="sandbox"),))

    with pytest.raises(NotFoundError):
        registry.resolve(Port.PROVIDER, "presidio", "2.1.0")


def test_an_empty_version_resolves_to_the_one_bindable_version() -> None:
    registry = Registry(
        (
            admitted(manifest(version="2.1.0"), suite_version="1", runner="sandbox"),
            # Pending, so not a candidate: an unadmitted version must not become
            # the answer just because it is the only other one.
            Component(manifest=manifest(version="3.0.0")),
        )
    )

    assert registry.resolve(Port.GUARDRAIL, "presidio", "").manifest.version == "2.1.0"


def test_an_ambiguous_empty_version_is_refused_rather_than_guessed() -> None:
    registry = Registry(
        (
            admitted(manifest(version="2.1.0"), suite_version="1", runner="sandbox"),
            admitted(manifest(version="3.0.0"), suite_version="1", runner="sandbox"),
        )
    )

    with pytest.raises(InvalidRequestError, match="more than one version"):
        registry.resolve(Port.GUARDRAIL, "presidio", "")


def test_registering_the_same_name_and_version_twice_is_refused() -> None:
    entry = admitted(manifest(), suite_version="1", runner="sandbox")

    with pytest.raises(InvalidRequestError, match="registered twice"):
        Registry((entry, entry))
