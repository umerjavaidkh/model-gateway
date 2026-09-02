"""The component registry: what may fill a port, and what proved it can.

A component is a manifest plus an admission record. The manifest is what the
publisher claims — name, version, the port it fills, the shape of its config,
what it costs, how it fails. The admission record is what a contract suite
observed, and it is the only thing that makes a manifest usable.

The registry exists so that binding a component to a port is a data question
rather than a deploy: tenant A gets Presidio, tenant B gets regex-only, tenant
C gets none, same binary. That is only safe if the set of bindable things is
governed, which is what this module is.

# Why admission is a stored record rather than a boolean

"Did it pass?" is not enough to act on a year later. Which suite, at which
version, run by what, against which artifact, and where is the evidence — those
are the questions asked during an incident or an audit, and a boolean answers
none of them. The record also makes re-admission on a suite change a visible
gap rather than an invisible one.

# What is deliberately not here

Executing the suite. Running a contract suite against a third-party component
means executing untrusted code, and that belongs in an ephemeral sandbox rather
than in the control plane's process. This module defines the record such a run
produces and refuses to invent one; see ``service/registry.py`` for the gate.
"""

from __future__ import annotations

import hashlib
import json
import re
from dataclasses import dataclass, field
from datetime import datetime
from enum import StrEnum
from typing import Any

from model_gateway_control.domain.signing import Signature
from model_gateway_control.errors import InvalidRequestError, NotFoundError

#: The official semver pattern, anchored. Versions are compared and displayed
#: but never ordered by string, because "1.10.0" sorts below "1.9.0".
_SEMVER = re.compile(
    r"^(?P<major>0|[1-9]\d*)\.(?P<minor>0|[1-9]\d*)\.(?P<patch>0|[1-9]\d*)"
    r"(?:-(?P<prerelease>(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)"
    r"(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?"
    r"(?:\+(?P<build>[0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$"
)

#: A component name is a slug: it appears in bindings, in metrics labels and in
#: log lines, so it may not carry whitespace or separators that would make any
#: of those ambiguous.
_NAME = re.compile(r"^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$")


class Port(StrEnum):
    """The extension surface a component fills.

    A closed set on purpose. Every port is a contract maintained forever and a
    compatibility surface for every component that fills it; the ceiling is the
    point, not an oversight.
    """

    PROVIDER = "provider"
    GUARDRAIL = "guardrail"
    STORE = "store"
    TELEMETRY = "telemetry"
    #: Control-plane ports. Same registry and same admission gate, but they
    #: never execute inside a request, so their latency budget is minutes.
    TRAINER = "trainer"
    EVAL = "eval"


#: Ports whose components run inside a request. Only these are subject to a
#: latency budget, because only these can make a caller wait.
REQUEST_PATH_PORTS = frozenset({Port.PROVIDER, Port.GUARDRAIL, Port.STORE, Port.TELEMETRY})


class FailureMode(StrEnum):
    """What to do when a component errors or exceeds its budget."""

    #: Allow the request. For detection that is useful but not authoritative.
    OPEN = "open"
    #: Refuse the request. For controls whose failure is not recoverable — a
    #: leaked credential cannot be un-leaked.
    CLOSED = "closed"


class Execution(StrEnum):
    """How a component runs, which decides how it is isolated."""

    #: WASM, in the worker's process. For anything sub-millisecond: routing
    #: strategies, deterministic detectors, cost calculators. Sandboxed by the
    #: runtime rather than by the operating system.
    IN_PROCESS = "in_process"
    #: A separate process on a Unix socket. For anything heavy or not written
    #: in Go, with its own resource limits and its own CVE surface.
    SIDECAR = "sidecar"
    #: Compiled into the worker. Our own adapters, which are governed by the
    #: same registry so that "what is bound" has one answer rather than two.
    BUILTIN = "builtin"


class Status(StrEnum):
    """Where a component is in its lifecycle."""

    #: Registered, not yet admitted. Cannot be bound.
    PENDING = "pending"
    #: Passed its contract suite and is bindable.
    ACTIVE = "active"
    #: Withdrawn. Existing snapshots that name it stay valid; new ones cannot
    #: bind it. Retiring rather than deleting keeps an audit trail of what was
    #: once running.
    RETIRED = "retired"


@dataclass(frozen=True, slots=True, kw_only=True)
class Manifest:
    """What a publisher claims about a component.

    Frozen and digested: the digest is what an admission record is bound to, so
    a manifest edited after admission no longer matches the thing that passed.
    """

    name: str
    version: str
    port: Port
    #: JSON Schema for this component's configuration, as text. Stored as text
    #: rather than a parsed object so the digest covers exactly what was
    #: submitted — a re-serialised schema is a different string.
    config_schema: str = "{}"
    #: What the component says it needs in the worst case. The registry checks
    #: bindings against it: a binding that allows less time than the component
    #: declares will time out on every request, which is a configuration error
    #: an operator should learn at build time rather than in production.
    latency_budget_ms: int = 0
    failure_mode: FailureMode = FailureMode.CLOSED
    execution: Execution = Execution.SIDECAR
    #: Free-form capability tags, e.g. "streaming", "network". Recorded so a
    #: deployment can refuse a component asking for something it will not grant.
    capabilities: tuple[str, ...] = ()
    #: For sidecar components, the pinned image. A digest rather than a tag: a
    #: floating tag turns an admitted artifact into a different one silently,
    #: which defeats the entire gate.
    image: str = ""
    #: For in-process components, the digest of the WASM module. There is no
    #: registry to pin against here — the artifact is a file — so the digest is
    #: over the bytes themselves, and a worker verifies it before compiling
    #: anything. Without that, the admission record vouches for one module and
    #: the worker runs whatever is on disk.
    module: str = ""

    def __post_init__(self) -> None:
        if not _NAME.match(self.name):
            raise InvalidRequestError(
                f"component name {self.name!r} must be a lowercase slug of 3 to 64 characters"
            )
        if not _SEMVER.match(self.version):
            raise InvalidRequestError(
                f"component {self.name!r} has version {self.version!r}, which is not semver"
            )
        if self.latency_budget_ms < 0:
            raise InvalidRequestError(f"component {self.name!r} has a negative latency budget")
        if self.port in REQUEST_PATH_PORTS and self.latency_budget_ms == 0:
            # Zero on a request-path port is not "fast", it is "unstated", and
            # an unstated budget is one nothing can be checked against.
            raise InvalidRequestError(
                f"component {self.name!r} fills {self.port} and must declare a latency budget"
            )
        if self.execution is Execution.SIDECAR and self.image and not _pinned(self.image):
            raise InvalidRequestError(
                f"component {self.name!r} pins {self.image!r} by tag; use an image digest"
            )
        if self.execution is Execution.IN_PROCESS:
            # Required rather than optional: an in-process component with no
            # module reference is one a worker cannot find, and it would fail
            # at load on every worker rather than at registration on one.
            if not self.module:
                raise InvalidRequestError(
                    f"component {self.name!r} runs in process and must name its module digest"
                )
            if not _DIGEST.match(self.module):
                raise InvalidRequestError(
                    f"component {self.name!r} has module {self.module!r}, "
                    "which is not a sha256 digest of the module bytes"
                )
        elif self.module:
            raise InvalidRequestError(
                f"component {self.name!r} names a module but does not run in process"
            )
        _validate_config_schema(self.name, self.config_schema)

    @property
    def ref(self) -> str:
        """How a component is named in a binding, a log line and an error."""
        return f"{self.name}@{self.version}"

    def digest(self) -> str:
        """A stable content digest over the manifest.

        Deterministic across processes and Python versions: sorted keys, no
        insignificant whitespace, UTF-8. This is what an admission record binds
        to, and it is what a signature would cover.
        """
        payload = json.dumps(
            {
                "name": self.name,
                "version": self.version,
                "port": str(self.port),
                "config_schema": self.config_schema,
                "latency_budget_ms": self.latency_budget_ms,
                "failure_mode": str(self.failure_mode),
                "execution": str(self.execution),
                "capabilities": sorted(self.capabilities),
                "image": self.image,
                "module": self.module,
            },
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=False,
        )
        return hashlib.sha256(payload.encode()).hexdigest()


#: A bare content address: sha256 followed by 64 hex digits.
_DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")


def _pinned(image: str) -> bool:
    """Report whether a reference names immutable content.

    Two forms qualify, and they are equally immutable. A repository digest —
    ``ghcr.io/acme/guard@sha256:...`` — is what a published component carries.
    A bare image ID — ``sha256:...`` — is what a locally built or air-gapped
    image has, because a repository digest only exists once something has been
    pushed to a registry; accepting only the first would mean an air-gapped
    deployment could admit nothing.

    A tag qualifies as neither: it names whatever was pushed most recently, so
    admitting an artifact by tag admits a different one tomorrow.

    The sandbox checks this again before running anything. Two checks of one
    rule is deliberate — this one stops a bad manifest being stored, and that
    one is the boundary that actually executes the bytes.
    """
    if _DIGEST.match(image):
        return True
    name, separator, digest = image.partition("@")
    return bool(separator and name and _DIGEST.match(digest))


def _validate_config_schema(name: str, schema: str) -> None:
    """Reject a config schema nothing could validate against.

    Deliberately shallow: this checks the schema is a JSON object, not that it
    is a valid draft-2020-12 document. Full meta-schema validation belongs with
    validating actual config payloads against it, and neither is useful without
    the other — a component's config is a reference today, not a document the
    control plane holds.
    """
    try:
        parsed = json.loads(schema)
    except json.JSONDecodeError as exc:
        raise InvalidRequestError(
            f"component {name!r} has a config schema that is not JSON"
        ) from exc
    if not isinstance(parsed, dict):
        raise InvalidRequestError(f"component {name!r} has a config schema that is not an object")


@dataclass(frozen=True, slots=True, kw_only=True)
class Admission:
    """The record of a contract suite run that admitted a component.

    Bound to ``manifest_digest`` rather than to a name and version, so editing
    an admitted manifest invalidates its admission instead of inheriting it.
    """

    #: Which port's suite ran, and at which version. A suite that gains a case
    #: is a new version, and every component admitted under the old one is
    #: visibly admitted under an older bar rather than silently grandfathered.
    suite: Port
    suite_version: str
    manifest_digest: str
    passed: bool
    #: What executed the suite. "The control plane ran it in-process" is a
    #: thing an auditor must be able to rule out, so the runner is named.
    runner: str
    #: Where the run's output lives. A verdict with no evidence is an opinion.
    evidence_ref: str = ""
    recorded_at: datetime | None = None

    def __post_init__(self) -> None:
        if not self.runner:
            raise InvalidRequestError("an admission must name what ran the suite")
        if not self.suite_version:
            raise InvalidRequestError("an admission must name the suite version that ran")
        if len(self.manifest_digest) != 64:
            raise InvalidRequestError("an admission must bind to a manifest digest")


@dataclass(frozen=True, slots=True, kw_only=True)
class Component:
    """A manifest and its lifecycle state."""

    manifest: Manifest
    status: Status = Status.PENDING
    admission: Admission | None = None
    #: The publisher signature submitted with this component, if any. Kept as
    #: evidence to be re-checked rather than as a claim that it was checked:
    #: a verification result stored in the database is only as trustworthy as
    #: the database, and the point of a signature is to not need that.
    signature: Signature | None = None

    def __post_init__(self) -> None:
        if self.status is Status.ACTIVE and not self.is_admitted:
            # The one invariant the whole module exists for. Anything that can
            # produce a Component enforces it here rather than at each caller.
            raise InvalidRequestError(
                f"component {self.manifest.ref} cannot be active without a passing admission "
                f"bound to its current manifest"
            )

    @property
    def is_admitted(self) -> bool:
        """Whether a passing suite run covers this exact manifest."""
        return (
            self.admission is not None
            and self.admission.passed
            and self.admission.manifest_digest == self.manifest.digest()
            and self.admission.suite == self.manifest.port
        )

    @property
    def is_bindable(self) -> bool:
        return self.status is Status.ACTIVE


@dataclass(frozen=True, slots=True)
class Registry:
    """The admitted set, as the snapshot builder sees it.

    A value rather than a service: a snapshot is built from one consistent view
    of the registry, and passing that view around is what keeps the builder a
    pure function of its inputs.
    """

    components: tuple[Component, ...] = ()
    _index: dict[tuple[Port, str, str], Component] = field(
        default_factory=dict, init=False, repr=False, compare=False
    )

    def __post_init__(self) -> None:
        index: dict[tuple[Port, str, str], Component] = {}
        for component in self.components:
            key = (component.manifest.port, component.manifest.name, component.manifest.version)
            if key in index:
                raise InvalidRequestError(f"component {component.manifest.ref} is registered twice")
            index[key] = component
        object.__setattr__(self, "_index", index)

    def resolve(self, port: Port, name: str, version: str) -> Component:
        """Find the component a binding names, or explain why it cannot.

        An empty version resolves to the single bindable version, and is a
        conflict when there is more than one — guessing which of two versions
        an operator meant is how the wrong one ends up in production.
        """
        if version:
            component = self._index.get((port, name, version))
            if component is None:
                raise NotFoundError(f"no {port} component {name}@{version} is registered")
            return component

        candidates = [
            c
            for c in self.components
            if c.manifest.port == port and c.manifest.name == name and c.is_bindable
        ]
        if not candidates:
            raise NotFoundError(f"no bindable {port} component named {name!r} is registered")
        if len(candidates) > 1:
            versions = ", ".join(sorted(c.manifest.version for c in candidates))
            raise InvalidRequestError(
                f"{name!r} is bindable at more than one version ({versions}); name one"
            )
        return candidates[0]

    def __len__(self) -> int:
        return len(self.components)


def admitted(
    manifest: Manifest,
    *,
    suite_version: str,
    runner: str,
    evidence_ref: str = "",
    passed: bool = True,
    signature: Signature | None = None,
) -> Component:
    """Build a component carrying an admission bound to this exact manifest.

    Records a verdict someone else reached; it does not reach one. The digest
    is computed here rather than passed in because a caller computing it by
    hand is a caller who can get it wrong, and the whole point of the binding
    is that it cannot be.
    """
    record = Admission(
        suite=manifest.port,
        suite_version=suite_version,
        manifest_digest=manifest.digest(),
        passed=passed,
        runner=runner,
        evidence_ref=evidence_ref,
    )
    return Component(
        manifest=manifest,
        status=Status.ACTIVE if passed else Status.PENDING,
        admission=record,
        signature=signature,
    )


def manifest_from_dict(raw: dict[str, Any]) -> Manifest:
    """Build a manifest from decoded JSON, rejecting unknown fields.

    Unknown fields are an error rather than ignored: a manifest with a
    misspelled ``latency_budget_ms`` would otherwise be admitted with the
    default, and the operator would have no way to tell.
    """
    known = set(Manifest.__dataclass_fields__)
    unknown = set(raw) - known
    if unknown:
        raise InvalidRequestError(f"unknown manifest field(s): {', '.join(sorted(unknown))}")

    try:
        return Manifest(
            name=str(raw["name"]),
            version=str(raw["version"]),
            port=Port(raw["port"]),
            config_schema=str(raw.get("config_schema", "{}")),
            latency_budget_ms=int(raw.get("latency_budget_ms", 0)),
            failure_mode=FailureMode(raw.get("failure_mode", "closed")),
            execution=Execution(raw.get("execution", "sidecar")),
            capabilities=tuple(str(c) for c in raw.get("capabilities", ())),
            image=str(raw.get("image", "")),
            module=str(raw.get("module", "")),
        )
    except KeyError as exc:
        raise InvalidRequestError(f"manifest is missing {exc.args[0]!r}") from exc
    except ValueError as exc:
        raise InvalidRequestError(f"manifest has an invalid value: {exc}") from exc
