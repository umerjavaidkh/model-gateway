"""SQLAlchemy models: the control plane's source of truth.

# Shape

    tenants ─┬─ key_prefixes
             ├─ orgs ── teams ─┬─ users ────┐
             │                 └─ apps ─────┤
             ├─ budgets                     └── api_keys ── key_budgets
             ├─ aliases ── alias_targets
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
    Boolean,
    DateTime,
    ForeignKey,
    Integer,
    LargeBinary,
    String,
    UniqueConstraint,
    func,
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
