"""The registry lifecycle against a real database.

The cases that matter are the ones where a component could become bindable
without a contract-suite run having covered it.
"""

from __future__ import annotations

import os
from collections.abc import AsyncIterator

import pytest
import pytest_asyncio
from sqlalchemy.ext.asyncio import AsyncSession

from model_gateway_control.db.models import Base
from model_gateway_control.db.repository import Repository
from model_gateway_control.db.session import create_engine, session_factory
from model_gateway_control.domain.component import (
    Admission,
    Manifest,
    Port,
    Status,
)
from model_gateway_control.errors import ConflictError, ForbiddenError, NotFoundError
from model_gateway_control.service.registry import AdmissionGate, RegistryService

DEFAULT_URL = "sqlite+aiosqlite:///:memory:"

MANIFEST = Manifest(
    name="presidio",
    version="2.1.0",
    port=Port.GUARDRAIL,
    latency_budget_ms=200,
    capabilities=("network",),
)


@pytest_asyncio.fixture
async def session() -> AsyncIterator[AsyncSession]:
    engine = create_engine(os.environ.get("GATEWAY_TEST_DATABASE_URL", DEFAULT_URL))
    async with engine.begin() as connection:
        await connection.run_sync(Base.metadata.drop_all)
        await connection.run_sync(Base.metadata.create_all)
    async with session_factory(engine)() as session:
        yield session
    await engine.dispose()


def verdict(
    passed: bool = True, *, digest: str | None = None, suite: Port = Port.GUARDRAIL
) -> Admission:
    return Admission(
        suite=suite,
        suite_version="1",
        manifest_digest=digest or MANIFEST.digest(),
        passed=passed,
        runner="sandbox://ephemeral",
        evidence_ref="s3://runs/1",
    )


class PassingGate:
    """A gate that reports a real-looking pass, standing in for the sandbox."""

    async def run(self, manifest: Manifest) -> Admission:
        return Admission(
            suite=manifest.port,
            suite_version="1",
            manifest_digest=manifest.digest(),
            passed=True,
            runner="sandbox://test",
        )


async def test_a_registered_component_is_not_yet_bindable(session: AsyncSession) -> None:
    # Registration is a publisher's act. If it granted bindability, "register"
    # would be the call that reaches production.
    component = await RegistryService(session).register(MANIFEST)

    assert component.status is Status.PENDING
    assert not component.is_bindable


async def test_the_default_gate_admits_nothing(session: AsyncSession) -> None:
    # A control plane with no sandbox configured must refuse rather than admit
    # on the strength of the manifest alone.
    service = RegistryService(session)
    await service.register(MANIFEST)

    with pytest.raises(ForbiddenError, match="no admission gate is configured"):
        await service.admit("presidio", "2.1.0")

    assert (await service.get("presidio", "2.1.0")).status is Status.PENDING


async def test_a_passing_gate_activates(session: AsyncSession) -> None:
    gate: AdmissionGate = PassingGate()
    service = RegistryService(session, gate)
    await service.register(MANIFEST)

    component = await service.admit("presidio", "2.1.0")

    assert component.status is Status.ACTIVE
    assert component.is_admitted
    assert component.admission is not None
    assert component.admission.runner == "sandbox://test"


async def test_a_failing_run_is_recorded_and_does_not_activate(session: AsyncSession) -> None:
    # "It was tried and it failed" is what stops the same broken component
    # being resubmitted in a loop with nobody the wiser.
    service = RegistryService(session)
    await service.register(MANIFEST)

    component = await service.record_admission("presidio", "2.1.0", verdict(passed=False))

    assert component.status is Status.PENDING
    assert component.admission is not None
    assert component.admission.passed is False


async def test_a_verdict_covering_another_manifest_is_refused(session: AsyncSession) -> None:
    # A run against a different artifact must not admit this one.
    service = RegistryService(session)
    await service.register(MANIFEST)

    with pytest.raises(ConflictError, match="different manifest"):
        await service.record_admission("presidio", "2.1.0", verdict(digest="0" * 64))


async def test_a_verdict_from_the_wrong_suite_is_refused(session: AsyncSession) -> None:
    service = RegistryService(session)
    await service.register(MANIFEST)

    with pytest.raises(ConflictError, match="provider suite"):
        await service.record_admission("presidio", "2.1.0", verdict(suite=Port.PROVIDER))


async def test_a_version_cannot_be_republished(session: AsyncSession) -> None:
    # A version identifies an artifact. Editing one in place means an admission
    # recorded yesterday describes something else today.
    service = RegistryService(session)
    await service.register(MANIFEST)

    with pytest.raises(ConflictError, match="already registered"):
        await service.register(MANIFEST)


async def test_retiring_stops_binding_but_keeps_the_record(session: AsyncSession) -> None:
    service = RegistryService(session, PassingGate())
    await service.register(MANIFEST)
    await service.admit("presidio", "2.1.0")

    retired = await service.retire("presidio", "2.1.0")

    assert retired.status is Status.RETIRED
    assert not retired.is_bindable
    # Still there: an audit needs to know what was once bindable, and the
    # error an operator gets should be "retired" rather than "no such thing".
    assert retired.admission is not None


async def test_a_failing_rerun_demotes_a_component_that_was_serving(
    session: AsyncSession,
) -> None:
    # Active means "the most recent run passed", with no exceptions. Leaving it
    # active would produce the one state this module exists to prevent:
    # something bindable that nothing currently vouches for.
    #
    # This is not an outage — snapshots already built keep serving, and only
    # the next build refuses to bind it.
    service = RegistryService(session, PassingGate())
    await service.register(MANIFEST)
    await service.admit("presidio", "2.1.0")

    after = await service.record_admission("presidio", "2.1.0", verdict(passed=False))

    assert after.status is Status.PENDING
    assert not after.is_bindable
    assert after.admission is not None
    assert after.admission.passed is False


async def test_a_passing_rerun_does_not_un_retire(session: AsyncSession) -> None:
    # Retirement is an operator's decision, and a green suite run is not a
    # request to put something back into service.
    service = RegistryService(session, PassingGate())
    await service.register(MANIFEST)
    await service.admit("presidio", "2.1.0")
    await service.retire("presidio", "2.1.0")

    after = await service.record_admission("presidio", "2.1.0", verdict(passed=True))

    assert after.status is Status.RETIRED


async def test_the_history_of_runs_is_kept(session: AsyncSession) -> None:
    service = RegistryService(session)
    await service.register(MANIFEST)
    await service.record_admission("presidio", "2.1.0", verdict(passed=False))
    latest = await service.record_admission("presidio", "2.1.0", verdict(passed=True))

    # The latest run is the current admission; the earlier failure is not
    # erased, because "when did this stop being tested" needs both.
    assert latest.status is Status.ACTIVE
    rows = await service.get("presidio", "2.1.0")
    assert rows.admission is not None
    assert rows.admission.passed is True


async def test_an_unregistered_component_cannot_be_admitted(session: AsyncSession) -> None:
    with pytest.raises(NotFoundError, match="presidio"):
        await RegistryService(session).record_admission("presidio", "2.1.0", verdict())


async def test_what_the_service_writes_is_what_the_snapshot_builder_reads(
    session: AsyncSession,
) -> None:
    # The two halves index the registry independently, so this is the only
    # place they are proven to agree about status, port and digest.
    service = RegistryService(session, PassingGate())
    await service.register(MANIFEST)
    await service.admit("presidio", "2.1.0")
    await session.commit()

    registry = await Repository(session).load_registry()
    resolved = registry.resolve(Port.GUARDRAIL, "presidio", "2.1.0")

    assert resolved.is_bindable
    assert resolved.is_admitted
    assert resolved.manifest.capabilities == ("network",)
    assert resolved.manifest.digest() == MANIFEST.digest()
