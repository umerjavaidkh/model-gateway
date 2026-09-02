"""Registering components, and the gate that decides which ones are bindable.

# The lifecycle

    register  ->  pending   (a manifest exists; nothing can bind it)
    admit     ->  active    (a contract suite passed against this exact manifest)
    retire    ->  retired   (no new snapshot may bind it; old ones stay valid)

Registration is deliberately not admission. Anyone may submit a manifest; only
a contract-suite run can make one bindable. Collapsing the two would make
"register" the code path that grants production access, which is precisely the
"remote-code-execution vulnerability with a nice admin UI" this design is meant
not to be.

# The gate

Admission needs a contract suite run against the actual component, which means
executing code the control plane did not write. That must not happen in this
process: it has database credentials, the key pepper, and the network position
of the thing that configures every worker.

So this module does not run suites. It defines ``AdmissionGate``, and its
default implementation refuses everything. A deployment that wants an open
registry must configure a real gate — an ephemeral, resource-limited, offline
sandbox — and the absence of one fails closed rather than fails open. See
``docs/adr/0009-component-registry.md``.
"""

from __future__ import annotations

import logging
from collections.abc import Sequence
from typing import Protocol

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db import models
from model_gateway_control.db.repository import to_component, to_manifest
from model_gateway_control.domain.component import (
    Admission,
    Component,
    Manifest,
    Port,
    Status,
)
from model_gateway_control.domain.signing import Signature, TrustStore
from model_gateway_control.errors import ConflictError, ForbiddenError, NotFoundError

_log = logging.getLogger(__name__)


class AdmissionGate(Protocol):
    """Runs a port's contract suite against a component and reports what happened.

    An implementation must execute the suite somewhere isolated from the
    control plane. The return value is a record of a real run: an
    implementation that fabricates a passing ``Admission`` without running
    anything defeats every other control in this module.
    """

    async def run(self, manifest: Manifest) -> Admission:
        """Run the suite for ``manifest.port`` and return what it observed."""


class NoGate:
    """The default gate: nothing is admitted.

    Refusing is the honest behaviour for a control plane with no sandbox
    configured. The alternative — admitting on the strength of the manifest
    alone — would make the registry a list of claims rather than a gate, and
    every binding downstream would inherit that.
    """

    async def run(self, manifest: Manifest) -> Admission:
        raise ForbiddenError(
            f"cannot admit {manifest.ref}: no admission gate is configured, so no contract "
            f"suite can be run. Configure a sandboxed runner, or record an admission from "
            f"one that already ran."
        )


class RegistryService:
    """Component lifecycle against the database.

    Takes a session rather than creating one, so a caller controls the
    transaction boundary — the same convention as the repository.
    """

    def __init__(
        self,
        session: AsyncSession,
        gate: AdmissionGate | None = None,
        trust: TrustStore | None = None,
    ) -> None:
        self._session = session
        self._gate = gate or NoGate()
        # An empty store with the default policy accepts unsigned components
        # and verifies any signature it is given against no keys — which means
        # it rejects every signature. That is the right default for a
        # deployment that has not set signing up: unsigned works, and a
        # signature nobody can check is not quietly treated as valid.
        self._trust = trust or TrustStore()

    async def register(self, manifest: Manifest, signature: Signature | None = None) -> Component:
        """Record a manifest. It is not bindable until it is admitted.

        Re-registering an existing name and version is a conflict rather than
        an update: a version is supposed to identify a artifact, and letting
        one be edited in place means an admission recorded yesterday describes
        something else today.
        """
        if await self._find(manifest.name, manifest.version) is not None:
            raise ConflictError(
                f"{manifest.ref} is already registered; publish a new version instead"
            )

        # Verified before anything is written. A publisher watching a failed
        # registration gets a clear reason here; the check that actually gates
        # production is the one the snapshot builder does, because a row
        # saying "this was verified" is only as good as the database.
        signer = self._trust.verify_for_registration(manifest.digest(), signature)

        row = models.Component(
            name=manifest.name,
            version=manifest.version,
            port=str(manifest.port),
            status=str(Status.PENDING),
            manifest_digest=manifest.digest(),
            config_schema=manifest.config_schema,
            latency_budget_ms=manifest.latency_budget_ms,
            failure_mode=str(manifest.failure_mode),
            execution=str(manifest.execution),
            image=manifest.image,
            module=manifest.module,
            signing_key_id=signature.key_id if signature else "",
            signature=signature.encoded() if signature else "",
            capabilities=[models.ComponentCapability(name=c) for c in manifest.capabilities],
        )
        self._session.add(row)
        await self._session.flush()
        if signer is not None:
            _log.info(
                "registered %s signed by %s (%s)", manifest.ref, signer.key_id, signer.publisher
            )
        return Component(manifest=manifest, status=Status.PENDING, signature=signature)

    async def admit(self, name: str, version: str) -> Component:
        """Run the configured gate and activate the component if it passes.

        A failing run is recorded too. "It was tried and it failed" is what
        stops the same broken component being resubmitted in a loop with
        nobody the wiser.
        """
        row = await self._require(name, version)
        manifest = to_manifest(row)

        admission = await self._gate.run(manifest)
        return await self._record(row, manifest, admission)

    async def record_admission(self, name: str, version: str, admission: Admission) -> Component:
        """Store an admission produced by a gate that ran elsewhere.

        The separate entry point exists because the suite runner is a separate
        deployable: it runs the sandbox, then reports. The verdict still has to
        bind to the manifest that was actually examined, which is checked here
        rather than trusted.
        """
        row = await self._require(name, version)
        return await self._record(row, to_manifest(row), admission)

    async def retire(self, name: str, version: str) -> Component:
        """Withdraw a component from new snapshots.

        The row stays. Existing snapshots that name it remain valid — workers
        already running it drain on the next version — and the audit trail of
        what was once bindable survives.
        """
        row = await self._require(name, version)
        row.status = str(Status.RETIRED)
        await self._session.flush()
        return to_component(row)

    async def get(self, name: str, version: str) -> Component:
        return to_component(await self._require(name, version))

    async def list(self, port: Port | None = None) -> Sequence[Component]:
        statement = select(models.Component).order_by(
            models.Component.name, models.Component.version
        )
        if port is not None:
            statement = statement.where(models.Component.port == str(port))
        rows = (await self._session.scalars(statement)).all()
        return [to_component(row) for row in rows]

    async def _record(
        self, row: models.Component, manifest: Manifest, admission: Admission
    ) -> Component:
        if admission.manifest_digest != manifest.digest():
            # The gate examined something else. Accepting this would admit an
            # artifact nothing tested, which is the one outcome the gate exists
            # to prevent.
            raise ConflictError(
                f"the admission for {manifest.ref} covers a different manifest; "
                f"it was edited after the suite ran"
            )
        if admission.suite != manifest.port:
            raise ConflictError(
                f"{manifest.ref} fills {manifest.port} but was tested against the "
                f"{admission.suite} suite"
            )

        row.admissions.append(
            models.ComponentAdmission(
                suite=str(admission.suite),
                suite_version=admission.suite_version,
                manifest_digest=admission.manifest_digest,
                passed=admission.passed,
                runner=admission.runner,
                evidence_ref=admission.evidence_ref,
            )
        )
        # The latest run is authoritative: active means "the most recent suite
        # run passed against this manifest", with no exceptions. Letting a
        # component stay active after a failing re-run would produce exactly
        # the state this module exists to prevent — something bindable that
        # nothing currently vouches for.
        #
        # Demotion is not an outage. Snapshots already built keep serving, and
        # workers keep running what they have; only the *next* build refuses to
        # bind it. A flaky suite therefore blocks the next configuration change
        # visibly rather than admitting something silently.
        #
        # Retirement is an operator's decision and outranks both: a passing run
        # does not un-retire, and a failing one does not need to.
        if row.status != str(Status.RETIRED):
            row.status = str(Status.ACTIVE if admission.passed else Status.PENDING)
        await self._session.flush()
        await self._session.refresh(row)
        return to_component(row)

    async def _find(self, name: str, version: str) -> models.Component | None:
        return (
            await self._session.scalars(
                select(models.Component).where(
                    models.Component.name == name, models.Component.version == version
                )
            )
        ).one_or_none()

    async def _require(self, name: str, version: str) -> models.Component:
        row = await self._find(name, version)
        if row is None:
            raise NotFoundError(f"no component {name}@{version} is registered")
        return row
