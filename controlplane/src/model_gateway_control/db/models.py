"""SQLAlchemy models: the control plane's source of truth.

# Shape

    tenants ─┬─ key_prefixes
             ├─ orgs ── teams ─┬─ users ────┐
             │                 └─ apps ─────┤
             ├─ budgets                     └── api_keys ── key_budgets
             ├─ aliases ── alias_targets
             ├─ plugin_bindings ──> components ─┬─ component_capabilities
             │                                  └─ component_admissions
             └─ (deployments and their capabilities are fleet-wide)

Ancestry is ordinary parent references rather than a materialized closure table.
See docs/adr/0005-no-closure-table.md: a closure table exists to avoid recursive
traversal on a latency-critical path, and nothing here is on one — the data
plane never reads a database at all.

# Portability

No Postgres-specific column types are used. The suite runs against SQLite for
fast local feedback and against Postgres in CI, and the schema stays expressible
in both. That is a constraint worth keeping: it rules out storing structure in
JSON columns, which is better modelling regardless of engine.
"""

from __future__ import annotations

from datetime import datetime

from sqlalchemy import (
    BigInteger,
    Boolean,
    DateTime,
    ForeignKey,
    Integer,
    LargeBinary,
    String,
    Text,
    UniqueConstraint,
    false,
    func,
    text,
)
from sqlalchemy.orm import DeclarativeBase, Mapped, mapped_column, relationship

#: Long enough for a UUID or a human-chosen slug, short enough to index.
ID_LENGTH = 64


class Base(DeclarativeBase):
    """Declarative base for every table."""


class TimestampMixin:
    """Creation and update stamps.

    Every row carries them because "when did this change" is the first question
    asked of a configuration store during an incident, and adding the columns
    afterwards means backfilling nulls that mean nothing.
    """

    created_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), server_default=func.now(), nullable=False
    )
    updated_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), server_default=func.now(), onupdate=func.now(), nullable=False
    )


class Tenant(Base, TimestampMixin):
    """A billing and isolation boundary. Owns one snapshot layer."""

    __tablename__ = "tenants"

    id: Mapped[str] = mapped_column(String(ID_LENGTH), primary_key=True)
    #: Plan tier. Safe as a metrics label, unlike the tenant id.
    tier: Mapped[str] = mapped_column(String(32), nullable=False, default="unknown")
    #: Floor on destination trust for every request from this tenant. Stored as
    #: the enum's integer so ordering comparisons work in SQL.
    min_trust_tier: Mapped[int] = mapped_column(Integer, nullable=False, default=1)
    #: Incremented whenever anything in this tenant's layer changes, which is
    #: what lets a worker reject an out-of-order snapshot.
    version: Mapped[int] = mapped_column(Integer, nullable=False, default=1)

    prefixes: Mapped[list[KeyPrefix]] = relationship(back_populates="tenant", lazy="selectin")
    orgs: Mapped[list[Org]] = relationship(back_populates="tenant", lazy="selectin")
    budgets: Mapped[list[Budget]] = relationship(back_populates="tenant", lazy="selectin")
    aliases: Mapped[list[Alias]] = relationship(back_populates="tenant", lazy="selectin")
    plugins: Mapped[list[PluginBinding]] = relationship(back_populates="tenant", lazy="selectin")


class KeyPrefix(Base, TimestampMixin):
    """Routes an API key's prefix segment to a tenant.

    The primary key is the prefix itself, so the database enforces what would
    otherwise be a silent authentication bug: two tenants claiming one prefix
    means one of them authenticates as the other.
    """

    __tablename__ = "key_prefixes"

    prefix: Mapped[str] = mapped_column(String(64), primary_key=True)
    tenant_id: Mapped[str] = mapped_column(
        ForeignKey("tenants.id", ondelete="CASCADE"), nullable=False, index=True
    )

    tenant: Mapped[Tenant] = relationship(back_populates="prefixes")


class Org(Base, TimestampMixin):
    """Top of a tenant's identity graph."""

    __tablename__ = "orgs"

    id: Mapped[str] = mapped_column(String(ID_LENGTH), primary_key=True)
    tenant_id: Mapped[str] = mapped_column(
        ForeignKey("tenants.id", ondelete="CASCADE"), nullable=False, index=True
    )
    name: Mapped[str] = mapped_column(String(255), nullable=False)

    tenant: Mapped[Tenant] = relationship(back_populates="orgs")
    teams: Mapped[list[Team]] = relationship(back_populates="org", lazy="selectin")


class Team(Base, TimestampMixin):
    """Groups users and applications within an org.

    ``parent_team_id`` is present and unused. It exists so that nesting is a
    migration rather than a redesign — and it is the trigger named in ADR 0005
    for revisiting the closure-table decision, because nesting is what makes
    depth unbounded.
    """

    __tablename__ = "teams"

    id: Mapped[str] = mapped_column(String(ID_LENGTH), primary_key=True)
    org_id: Mapped[str] = mapped_column(
        ForeignKey("orgs.id", ondelete="CASCADE"), nullable=False, index=True
    )
    parent_team_id: Mapped[str | None] = mapped_column(
        ForeignKey("teams.id", ondelete="SET NULL"), nullable=True
    )
    name: Mapped[str] = mapped_column(String(255), nullable=False)

    org: Mapped[Org] = relationship(back_populates="teams")
    users: Mapped[list[User]] = relationship(back_populates="team", lazy="selectin")
    applications: Mapped[list[Application]] = relationship(back_populates="team", lazy="selectin")


class User(Base, TimestampMixin):
    """A human principal, usually authenticated by OIDC."""

    __tablename__ = "users"

    id: Mapped[str] = mapped_column(String(ID_LENGTH), primary_key=True)
    team_id: Mapped[str] = mapped_column(
        ForeignKey("teams.id", ondelete="CASCADE"), nullable=False, index=True
    )
    subject: Mapped[str] = mapped_column(String(255), nullable=False)

    team: Mapped[Team] = relationship(back_populates="users")


class Application(Base, TimestampMixin):
    """A service principal, usually authenticated by API key."""

    __tablename__ = "applications"

    id: Mapped[str] = mapped_column(String(ID_LENGTH), primary_key=True)
    team_id: Mapped[str] = mapped_column(
        ForeignKey("teams.id", ondelete="CASCADE"), nullable=False, index=True
    )
    name: Mapped[str] = mapped_column(String(255), nullable=False)

    team: Mapped[Team] = relationship(back_populates="applications")


class ApiKey(Base, TimestampMixin):
    """One issued credential.

    What is stored is ``lookup`` — HMAC-SHA256 of the secret under a pepper the
    database never sees. The secret itself exists in clear text once, at
    issuance, and is never written anywhere.

    A key belongs to an application or a user, not both, and the check is left
    to the repository rather than a database constraint because the two engines
    express partial constraints differently and the rule is cheap to state in
    one place.
    """

    __tablename__ = "api_keys"
    __table_args__ = (UniqueConstraint("lookup", name="uq_api_keys_lookup"),)

    id: Mapped[str] = mapped_column(String(ID_LENGTH), primary_key=True)
    tenant_id: Mapped[str] = mapped_column(
        ForeignKey("tenants.id", ondelete="CASCADE"), nullable=False, index=True
    )
    application_id: Mapped[str | None] = mapped_column(
        ForeignKey("applications.id", ondelete="CASCADE"), nullable=True, index=True
    )
    user_id: Mapped[str | None] = mapped_column(
        ForeignKey("users.id", ondelete="CASCADE"), nullable=True, index=True
    )

    lookup: Mapped[bytes] = mapped_column(LargeBinary(32), nullable=False)

    models_allow_all: Mapped[bool] = mapped_column(Boolean, nullable=False, default=False)
    default_data_class: Mapped[str] = mapped_column(String(32), nullable=False, default="")
    min_trust_tier: Mapped[int] = mapped_column(Integer, nullable=False, default=1)
    # Zero means unlimited for each dimension, so a key created before limits
    # existed is not silently capped at nothing.
    requests_per_minute: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    tokens_per_minute: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    max_concurrent: Mapped[int] = mapped_column(Integer, nullable=False, default=0)

    #: The outgoing generation of a rotated key stays usable until ``not_after``,
    #: which is what makes rotation a non-event for callers.
    deprecated: Mapped[bool] = mapped_column(Boolean, nullable=False, default=False)
    not_after: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), nullable=True)
    #: Set when a key is revoked outright. A revoked key is excluded from the
    #: next snapshot rather than deleted, so the audit trail survives.
    revoked_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), nullable=True)

    application: Mapped[Application | None] = relationship(lazy="selectin")
    user: Mapped[User | None] = relationship(lazy="selectin")
    roles: Mapped[list[KeyRole]] = relationship(lazy="selectin", cascade="all, delete-orphan")
    models: Mapped[list[KeyModel]] = relationship(lazy="selectin", cascade="all, delete-orphan")
    budgets: Mapped[list[KeyBudget]] = relationship(lazy="selectin", cascade="all, delete-orphan")


class KeyRole(Base):
    """A role granted to a key. Its own table rather than a delimited string."""

    __tablename__ = "key_roles"

    key_id: Mapped[str] = mapped_column(
        ForeignKey("api_keys.id", ondelete="CASCADE"), primary_key=True
    )
    role: Mapped[str] = mapped_column(String(64), primary_key=True)


class KeyModel(Base):
    """One entry in a key's model allowlist.

    An empty allowlist means *nothing*, not everything — ``models_allow_all`` on
    the key is the explicit opt-out, so a key created without any rows fails
    closed.
    """

    __tablename__ = "key_models"

    key_id: Mapped[str] = mapped_column(
        ForeignKey("api_keys.id", ondelete="CASCADE"), primary_key=True
    )
    model: Mapped[str] = mapped_column(String(255), primary_key=True)


class Budget(Base, TimestampMixin):
    """A spend limit, and what has been spent against it.

    ``spent_micro_usd`` is updated by the accounting consumer from the usage
    stream, which is why budgets are eventually consistent by design and rate
    limits are the mechanism for anything that must be immediate.
    """

    __tablename__ = "budgets"

    id: Mapped[str] = mapped_column(String(ID_LENGTH), primary_key=True)
    tenant_id: Mapped[str] = mapped_column(
        ForeignKey("tenants.id", ondelete="CASCADE"), nullable=False, index=True
    )
    scope: Mapped[int] = mapped_column(Integer, nullable=False)
    limit_micro_usd: Mapped[int] = mapped_column(Integer, nullable=False)
    spent_micro_usd: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    hard: Mapped[bool] = mapped_column(Boolean, nullable=False, default=True)
    headroom_basis_points: Mapped[int] = mapped_column(Integer, nullable=False, default=500)

    tenant: Mapped[Tenant] = relationship(back_populates="budgets")


class KeyBudget(Base):
    """Attaches a budget to a key.

    The full chain a request must satisfy — key, app, user, team, org, model —
    is assembled by the builder. This table records the explicit attachments;
    the inherited ones come from walking the hierarchy.
    """

    __tablename__ = "key_budgets"

    key_id: Mapped[str] = mapped_column(
        ForeignKey("api_keys.id", ondelete="CASCADE"), primary_key=True
    )
    budget_id: Mapped[str] = mapped_column(
        ForeignKey("budgets.id", ondelete="CASCADE"), primary_key=True
    )


class Deployment(Base, TimestampMixin):
    """One reachable endpoint that can serve a routing key.

    Fleet-wide rather than per-tenant: the catalog is the large, slowly-changing
    half of a snapshot, and duplicating it per tenant is exactly what the
    layered design avoids.
    """

    __tablename__ = "deployments"

    id: Mapped[str] = mapped_column(String(ID_LENGTH), primary_key=True)
    base_model: Mapped[str] = mapped_column(String(255), nullable=False, index=True)
    #: Empty for a plain base model. Part of the routing key from the first
    #: release, because multi-LoRA serving means one base deployment serves many
    #: adapters and retrofitting the key later is a migration.
    adapter_id: Mapped[str] = mapped_column(String(255), nullable=False, default="")
    provider: Mapped[str] = mapped_column(String(64), nullable=False)
    endpoint: Mapped[str] = mapped_column(String(1024), nullable=False)
    region: Mapped[str] = mapped_column(String(64), nullable=False, default="")
    trust_tier: Mapped[int] = mapped_column(Integer, nullable=False)
    #: A reference to a secret, never the secret.
    credential_ref: Mapped[str] = mapped_column(String(512), nullable=False, default="")
    #: 0-100. Zero means registered but not serving.
    weight: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    input_cost_micro_usd: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    output_cost_micro_usd: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    # Zero means unconfigured, and the data plane falls back to the standard
    # input rate rather than billing these at nothing.
    cached_input_cost_micro_usd: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    cache_write_cost_micro_usd: Mapped[int] = mapped_column(Integer, nullable=False, default=0)

    capabilities: Mapped[list[DeploymentCapability]] = relationship(
        lazy="selectin", cascade="all, delete-orphan"
    )


class DeploymentCapability(Base):
    """A capability a deployment declares."""

    __tablename__ = "deployment_capabilities"

    deployment_id: Mapped[str] = mapped_column(
        ForeignKey("deployments.id", ondelete="CASCADE"), primary_key=True
    )
    capability: Mapped[str] = mapped_column(String(64), primary_key=True)


class Alias(Base, TimestampMixin):
    """A model alias.

    ``tenant_id`` is null for a fleet-wide alias and set for a tenant override.
    One table rather than two, because they are the same thing resolved at
    different layers, and splitting them would duplicate the target table too.
    """

    __tablename__ = "aliases"
    __table_args__ = (UniqueConstraint("tenant_id", "name", name="uq_aliases_tenant_name"),)

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    tenant_id: Mapped[str | None] = mapped_column(
        ForeignKey("tenants.id", ondelete="CASCADE"), nullable=True, index=True
    )
    name: Mapped[str] = mapped_column(String(255), nullable=False)

    tenant: Mapped[Tenant | None] = relationship(back_populates="aliases")
    targets: Mapped[list[AliasTarget]] = relationship(
        lazy="selectin", cascade="all, delete-orphan", order_by="AliasTarget.position"
    )


class AliasTarget(Base):
    """One target of an alias, in preference order."""

    __tablename__ = "alias_targets"

    alias_id: Mapped[int] = mapped_column(
        ForeignKey("aliases.id", ondelete="CASCADE"), primary_key=True
    )
    position: Mapped[int] = mapped_column(Integer, primary_key=True)
    base_model: Mapped[str] = mapped_column(String(255), nullable=False)
    adapter_id: Mapped[str] = mapped_column(String(255), nullable=False, default="")


class PluginBinding(Base, TimestampMixin):
    """Which registry component fills a port.

    ``tenant_id`` null is the fleet default; set is a tenant override. This is
    what makes "tenant A gets Presidio, tenant B gets regex-only, tenant C gets
    none, same binary" a data question.
    """

    __tablename__ = "plugin_bindings"
    __table_args__ = (UniqueConstraint("tenant_id", "port", name="uq_plugin_bindings_tenant_port"),)

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    tenant_id: Mapped[str | None] = mapped_column(
        ForeignKey("tenants.id", ondelete="CASCADE"), nullable=True, index=True
    )
    port: Mapped[str] = mapped_column(String(32), nullable=False)
    component: Mapped[str] = mapped_column(String(255), nullable=False)
    version: Mapped[str] = mapped_column(String(64), nullable=False, default="")
    config_ref: Mapped[str] = mapped_column(String(512), nullable=False, default="")

    tenant: Mapped[Tenant | None] = relationship(back_populates="plugins")


class Component(Base, TimestampMixin):
    """A registered component: what a publisher claims it is.

    ``manifest_digest`` is stored rather than recomputed on read, because it is
    what an admission binds to. Recomputing it would make an admission valid
    again after an edit that should have invalidated it.
    """

    __tablename__ = "components"
    __table_args__ = (UniqueConstraint("name", "version", name="uq_components_name_version"),)

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    name: Mapped[str] = mapped_column(String(64), nullable=False, index=True)
    version: Mapped[str] = mapped_column(String(64), nullable=False)
    port: Mapped[str] = mapped_column(String(32), nullable=False, index=True)
    status: Mapped[str] = mapped_column(String(16), nullable=False, default="pending")
    manifest_digest: Mapped[str] = mapped_column(String(64), nullable=False)
    #: The submitted config schema, verbatim. Text rather than a JSON column so
    #: the schema stays expressible on both SQLite and Postgres, and so the
    #: digest covers exactly the bytes that were submitted.
    config_schema: Mapped[str] = mapped_column(Text, nullable=False, default="{}")
    latency_budget_ms: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    failure_mode: Mapped[str] = mapped_column(String(16), nullable=False, default="closed")
    execution: Mapped[str] = mapped_column(String(16), nullable=False, default="sidecar")
    image: Mapped[str] = mapped_column(String(512), nullable=False, default="")
    #: sha256:<64 hex> of the WASM module, for in-process components.
    #:
    #: server_default as well as default, matching the migration that added it:
    #: without it every autogenerate run proposes dropping the default that is
    #: actually in the database.
    module: Mapped[str] = mapped_column(
        String(128), nullable=False, default="", server_default=text("''")
    )
    #: The publisher key that signed this manifest, and the signature itself as
    #: base64. Stored so it can be re-verified against the configured trust
    #: root at any time — the row is evidence, not a verdict.
    signing_key_id: Mapped[str] = mapped_column(
        String(128), nullable=False, default="", server_default=text("''")
    )
    signature: Mapped[str] = mapped_column(
        String(256), nullable=False, default="", server_default=text("''")
    )

    capabilities: Mapped[list[ComponentCapability]] = relationship(
        back_populates="component", cascade="all, delete-orphan", lazy="selectin"
    )
    admissions: Mapped[list[ComponentAdmission]] = relationship(
        back_populates="component",
        cascade="all, delete-orphan",
        lazy="selectin",
        order_by="ComponentAdmission.id",
    )


class ComponentCapability(Base):
    """One capability a component declares it needs.

    A child table rather than a delimited string, for the same reason
    deployment capabilities are: a delimited string cannot be queried, and
    "which components ask for network access" is a question worth asking.
    """

    __tablename__ = "component_capabilities"
    __table_args__ = (UniqueConstraint("component_id", "name", name="uq_component_capabilities"),)

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    component_id: Mapped[int] = mapped_column(
        ForeignKey("components.id", ondelete="CASCADE"), nullable=False, index=True
    )
    name: Mapped[str] = mapped_column(String(64), nullable=False)

    component: Mapped[Component] = relationship(back_populates="capabilities")


class ComponentAdmission(Base):
    """One contract-suite run against one component.

    Append-only: runs are never updated, and the latest row for a component is
    its current admission. Overwriting would erase the history that makes
    "when did this stop being tested" answerable — and a failing re-run is
    exactly the row worth keeping.
    """

    __tablename__ = "component_admissions"

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    component_id: Mapped[int] = mapped_column(
        ForeignKey("components.id", ondelete="CASCADE"), nullable=False, index=True
    )
    suite: Mapped[str] = mapped_column(String(32), nullable=False)
    suite_version: Mapped[str] = mapped_column(String(64), nullable=False)
    #: The manifest this run actually examined. An admission whose digest no
    #: longer matches the component's manifest does not admit it.
    manifest_digest: Mapped[str] = mapped_column(String(64), nullable=False)
    passed: Mapped[bool] = mapped_column(Boolean, nullable=False)
    #: What executed the suite. "The control plane ran it in-process" has to be
    #: something an auditor can rule out.
    runner: Mapped[str] = mapped_column(String(255), nullable=False)
    evidence_ref: Mapped[str] = mapped_column(String(512), nullable=False, default="")
    recorded_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), server_default=func.now(), nullable=False
    )

    component: Mapped[Component] = relationship(back_populates="admissions")


class FleetState(Base, TimestampMixin):
    """A single row holding the fleet-wide snapshot version.

    A table rather than a counter in code, because the version has to survive a
    restart and be monotonic across every control-plane replica. The primary key
    is fixed at 1 so a second row cannot exist.
    """

    __tablename__ = "fleet_state"

    id: Mapped[int] = mapped_column(Integer, primary_key=True, default=1)
    version: Mapped[int] = mapped_column(Integer, nullable=False, default=1)
    policy_bundle_ref: Mapped[str] = mapped_column(String(255), nullable=False, default="")


class IdempotencyRecord(Base):
    """A completed mutation, keyed by the caller's idempotency key.

    The plan calls for idempotency on every mutation because an agent drives
    this API, and an agent that cannot tell a timeout from a failure will retry.
    Without this table a retried "issue a key" quietly issues two, and the first
    one is never returned to anybody.

    ``request_fingerprint`` is stored so that reusing a key with a *different*
    body is a conflict rather than a silent replay of the wrong response — which
    would be worse than no idempotency at all.
    """

    __tablename__ = "idempotency_records"

    key: Mapped[str] = mapped_column(String(255), primary_key=True)
    endpoint: Mapped[str] = mapped_column(String(255), primary_key=True)
    request_fingerprint: Mapped[str] = mapped_column(String(64), nullable=False)
    status_code: Mapped[int] = mapped_column(Integer, nullable=False)
    response_body: Mapped[str] = mapped_column(Text, nullable=False)
    created_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), server_default=func.now(), nullable=False
    )


class UsageRecord(Base):
    """One request, recorded once.

    The stream is at-least-once, so the consumer sees duplicates after any
    restart or redelivery. This table is what makes it idempotent: the request
    id is the primary key, an insert that conflicts is discarded, and spend is
    applied only for inserts that actually happened.

    It doubles as the audit trail for what a budget was charged. Without it,
    "why is this tenant's spend what it is" has no answer beyond a running
    total that cannot be recomputed.
    """

    __tablename__ = "usage_records"

    request_id: Mapped[str] = mapped_column(String(128), primary_key=True)
    tenant_id: Mapped[str] = mapped_column(String(ID_LENGTH), nullable=False, index=True)
    key_id: Mapped[str] = mapped_column(String(ID_LENGTH), nullable=False, index=True)
    occurred_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), nullable=False)

    input_tokens: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    cached_input_tokens: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    cache_write_tokens: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    output_tokens: Mapped[int] = mapped_column(Integer, nullable=False, default=0)

    cost_micro_usd: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    price_micro_usd: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    outcome: Mapped[str] = mapped_column(String(64), nullable=False, default="")

    #: Which deployment served it. Indexed because the question this exists to
    #: answer — is this canary healthy — is asked per deployment over a window,
    #: and without attribution it cannot be asked at all.
    deployment: Mapped[str] = mapped_column(
        String(128), nullable=False, index=True, server_default=text("''")
    )
    #: Whether this was a mirrored request nobody was waiting for. Kept apart
    #: from real traffic: a shadow's errors are a finding about an adapter, not
    #: an incident about a tenant's requests.
    shadow: Mapped[bool] = mapped_column(Boolean, nullable=False, server_default=false())

    #: What the request actually did, kept so a failure can be read without a
    #: trace backend. The event carried all of this already and the consumer
    #: was throwing it away.
    base_model: Mapped[str] = mapped_column(String(255), nullable=False, server_default=text("''"))
    adapter_id: Mapped[str] = mapped_column(String(255), nullable=False, server_default=text("''"))
    provider: Mapped[str] = mapped_column(String(64), nullable=False, server_default=text("''"))
    stream: Mapped[bool] = mapped_column(Boolean, nullable=False, server_default=false())
    latency_ms: Mapped[int] = mapped_column(Integer, nullable=False, server_default=text("0"))
    time_to_first_byte_ms: Mapped[int] = mapped_column(
        Integer, nullable=False, server_default=text("0")
    )
    snapshot_version: Mapped[int] = mapped_column(
        BigInteger, nullable=False, server_default=text("0")
    )
    #: Per-stage timings as JSON, in the order they ran.
    #:
    #: JSON rather than a stages table: they are written once with the request,
    #: read whole when somebody opens that request, and never queried across. A
    #: join per row on a table this size would cost far more than it returns.
    stages: Mapped[str] = mapped_column(Text, nullable=False, server_default=text("'[]'"))


class PolicyRule(Base, TimestampMixin):
    """One rule of a tenant's policy, in evaluation order.

    ``tenant_id`` is null for the fleet default and set for a tenant's own.
    ``position`` is the evaluation order, which is the whole of the
    conflict-resolution semantics — first match wins, so the order an operator
    wrote is what they get.
    """

    __tablename__ = "policy_rules"
    __table_args__ = (UniqueConstraint("tenant_id", "rule_id", name="uq_policy_rules_id"),)

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    tenant_id: Mapped[str | None] = mapped_column(
        ForeignKey("tenants.id", ondelete="CASCADE"), nullable=True, index=True
    )
    rule_id: Mapped[str] = mapped_column(String(ID_LENGTH), nullable=False)
    position: Mapped[int] = mapped_column(Integer, nullable=False)
    effect: Mapped[str] = mapped_column(String(16), nullable=False)

    max_payload_bytes: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    data_class: Mapped[str] = mapped_column(String(32), nullable=False, default="")
    min_trust_tier: Mapped[int] = mapped_column(Integer, nullable=False, default=0)
    #: Returned to a caller on refusal, so operators write it and nothing
    #: derives it from the payload.
    reason: Mapped[str] = mapped_column(String(512), nullable=False, default="")

    conditions: Mapped[list[PolicyCondition]] = relationship(
        lazy="selectin", cascade="all, delete-orphan"
    )


class PolicyCondition(Base):
    """One value a rule tests against.

    A single table with a ``kind`` column rather than one table per condition
    type: the five kinds have identical shape, and five tables would be five
    joins to read one rule.
    """

    __tablename__ = "policy_conditions"

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    rule_id: Mapped[int] = mapped_column(
        ForeignKey("policy_rules.id", ondelete="CASCADE"), nullable=False, index=True
    )
    #: model | endpoint | role | region | cidr
    kind: Mapped[str] = mapped_column(String(16), nullable=False)
    value: Mapped[str] = mapped_column(String(255), nullable=False)


class FineTuneJob(Base, TimestampMixin):
    """A fine-tuning job: the spec that was submitted and where it has got to.

    Spec and status in one row rather than two tables. They are written
    together, read together, and locked together by the reconciler; splitting
    them would buy a join and a way for the two halves to disagree about which
    generation of a job is being reconciled.
    """

    __tablename__ = "finetune_jobs"
    __table_args__ = (
        UniqueConstraint("tenant_id", "name", name="uq_finetune_jobs_name"),
        # Unique across every job, not per tenant. Two jobs sharing a key would
        # get the same run back from an idempotent trainer, so the second would
        # silently adopt the first one's training run and its artifact.
        UniqueConstraint("idempotency_key", name="uq_finetune_jobs_idempotency_key"),
    )

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    tenant_id: Mapped[str] = mapped_column(
        String(64), ForeignKey("tenants.id", ondelete="CASCADE"), index=True, nullable=False
    )
    name: Mapped[str] = mapped_column(String(64), nullable=False)

    # --- spec: what was asked for, immutable once submitted ---
    base_model: Mapped[str] = mapped_column(String(128), nullable=False)
    trainer: Mapped[str] = mapped_column(String(64), nullable=False)
    trainer_version: Mapped[str] = mapped_column(String(64), nullable=False)
    dataset_uri: Mapped[str] = mapped_column(String(1024), nullable=False)
    dataset_checksum: Mapped[str] = mapped_column(String(128), nullable=False)
    dataset_rows: Mapped[int] = mapped_column(Integer, nullable=False)
    dataset_schema_version: Mapped[str] = mapped_column(String(64), nullable=False)
    #: Opaque to the gateway, so stored as JSON text rather than columns: every
    #: backend has its own, and a schema here would be a lowest common
    #: denominator that blocks the interesting ones.
    hyperparameters: Mapped[str] = mapped_column(Text, nullable=False, server_default=text("'{}'"))
    budget_ref: Mapped[str] = mapped_column(String(64), nullable=False, server_default=text("''"))
    eval_suite: Mapped[str] = mapped_column(String(64), nullable=False, server_default=text("''"))

    #: Generated once at creation and sent with every submission attempt. This
    #: is what lets a reconciler that crashed mid-submit ask the trainer what
    #: happened instead of guessing — and guessing wrong books a second run.
    idempotency_key: Mapped[str] = mapped_column(String(64), nullable=False)

    # --- status: where it has got to, maintained by the reconciler ---
    phase: Mapped[str] = mapped_column(String(32), nullable=False, index=True)
    external_id: Mapped[str] = mapped_column(String(255), nullable=False, server_default=text("''"))
    artifact_ref: Mapped[str] = mapped_column(
        String(512), nullable=False, server_default=text("''")
    )
    reason: Mapped[str] = mapped_column(Text, nullable=False, server_default=text("''"))
    #: Integer micro-USD, like every other amount in this system. Never a float.
    cost_micro_usd: Mapped[int] = mapped_column(
        BigInteger, nullable=False, server_default=text("0")
    )
    attempts: Mapped[int] = mapped_column(Integer, nullable=False, server_default=text("0"))

    #: The promotion gate, fixed at submission so lowering it later cannot
    #: retroactively promote something that already failed.
    gate_min_score: Mapped[int] = mapped_column(Integer, nullable=False, server_default=text("0"))
    gate_must_not_regress: Mapped[str] = mapped_column(
        Text, nullable=False, server_default=text("'[]'")
    )
    #: What the suite measured, as JSON. Stored whole rather than as rows in a
    #: metrics table: a scorecard is written once, read once, and only ever
    #: compared against another scorecard — there is nothing to query across.
    scorecard: Mapped[str] = mapped_column(Text, nullable=False, server_default=text("''"))
    baseline: Mapped[str] = mapped_column(Text, nullable=False, server_default=text("''"))

    #: Where the adapter is in its canary walk. -1 means no rollout has been
    #: started, which is distinct from step 0 at weight 0.
    rollout_step: Mapped[int] = mapped_column(Integer, nullable=False, server_default=text("-1"))
    rollout_weight: Mapped[int] = mapped_column(Integer, nullable=False, server_default=text("0"))
    #: The steps to walk, as a JSON list of percentages. On the job rather than
    #: in configuration, so the rollout a job gets is the one it was submitted
    #: with.
    canary_steps: Mapped[str] = mapped_column(
        Text, nullable=False, server_default=text("'[1, 5, 25, 100]'")
    )
    #: Share of the base model's traffic mirrored while the adapter is at zero
    #: weight.
    shadow_percent: Mapped[int] = mapped_column(Integer, nullable=False, server_default=text("10"))


class AuditRecord(Base):
    """One decision, appended and never changed.

    Append-only is a property of the code, not of the table: Postgres has no
    "insert only" mode a migration could switch on, and a role that can write
    can rewrite. What the schema contributes is the chain — ``prev_hash`` links
    each row to the one before it, so a deletion or an edit is detectable by
    anyone who recomputes it. Enforcement of *who may write* belongs to the
    database's grants, and is deployment configuration.

    ``seq`` rather than a timestamp orders the chain. Timestamps come from the
    data plane's clock, several of them, and two workers can produce records a
    millisecond apart in the wrong order — which would make a correct chain
    look broken. The consumer assigns ``seq`` as it appends, and it is the only
    writer.
    """

    __tablename__ = "audit_records"

    seq: Mapped[int] = mapped_column(BigInteger, primary_key=True, autoincrement=False)

    #: What makes redelivery safe. The stream is at-least-once, so the same
    #: decision arrives more than once after any restart; a unique event id is
    #: what lets the appender recognise it rather than extending the chain
    #: twice with the same fact.
    event_id: Mapped[str] = mapped_column(String(255), nullable=False, unique=True)

    #: Empty for decisions that were not a request — a key issued, a policy
    #: published.
    request_id: Mapped[str] = mapped_column(String(128), nullable=False, default="")
    occurred_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), nullable=False, index=True
    )

    tenant_id: Mapped[str] = mapped_column(String(ID_LENGTH), nullable=False, index=True)
    #: Who did it: a key id, a user subject, or a service-account name.
    actor: Mapped[str] = mapped_column(String(255), nullable=False, default="")
    #: What they did. Indexed because every audit question starts by narrowing
    #: to a kind of action.
    action: Mapped[str] = mapped_column(String(128), nullable=False, index=True)
    #: What they did it to.
    resource: Mapped[str] = mapped_column(String(255), nullable=False, default="")
    #: Empty when the action was allowed, otherwise the code it was refused
    #: with.
    outcome: Mapped[str] = mapped_column(String(64), nullable=False, default="")
    #: The structured half of why. Never the payload — the audit tap sits after
    #: redaction so that this table does not become a copy of the data it
    #: exists to protect.
    reason: Mapped[str] = mapped_column(Text, nullable=False, default="")
    source_ip: Mapped[str] = mapped_column(String(64), nullable=False, default="")
    #: The configuration in force when the decision was made, which is what
    #: makes "why was this allowed in March" answerable.
    snapshot_version: Mapped[int] = mapped_column(BigInteger, nullable=False, default=0)

    prev_hash: Mapped[str] = mapped_column(String(64), nullable=False)
    hash: Mapped[str] = mapped_column(String(64), nullable=False)
