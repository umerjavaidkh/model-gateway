from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Iterable as _Iterable, Mapping as _Mapping, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class TrustTier(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    TRUST_TIER_UNSPECIFIED: _ClassVar[TrustTier]
    TRUST_TIER_EXTERNAL: _ClassVar[TrustTier]
    TRUST_TIER_PRIVATE_CLOUD: _ClassVar[TrustTier]
    TRUST_TIER_INTERNAL: _ClassVar[TrustTier]

class BudgetScope(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    BUDGET_SCOPE_UNSPECIFIED: _ClassVar[BudgetScope]
    BUDGET_SCOPE_KEY: _ClassVar[BudgetScope]
    BUDGET_SCOPE_APP: _ClassVar[BudgetScope]
    BUDGET_SCOPE_USER: _ClassVar[BudgetScope]
    BUDGET_SCOPE_TEAM: _ClassVar[BudgetScope]
    BUDGET_SCOPE_ORG: _ClassVar[BudgetScope]
    BUDGET_SCOPE_MODEL: _ClassVar[BudgetScope]
    BUDGET_SCOPE_TRAINING: _ClassVar[BudgetScope]

class Port(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    PORT_UNSPECIFIED: _ClassVar[Port]
    PORT_PROVIDER: _ClassVar[Port]
    PORT_GUARDRAIL: _ClassVar[Port]
    PORT_STORE: _ClassVar[Port]
    PORT_TELEMETRY: _ClassVar[Port]
TRUST_TIER_UNSPECIFIED: TrustTier
TRUST_TIER_EXTERNAL: TrustTier
TRUST_TIER_PRIVATE_CLOUD: TrustTier
TRUST_TIER_INTERNAL: TrustTier
BUDGET_SCOPE_UNSPECIFIED: BudgetScope
BUDGET_SCOPE_KEY: BudgetScope
BUDGET_SCOPE_APP: BudgetScope
BUDGET_SCOPE_USER: BudgetScope
BUDGET_SCOPE_TEAM: BudgetScope
BUDGET_SCOPE_ORG: BudgetScope
BUDGET_SCOPE_MODEL: BudgetScope
BUDGET_SCOPE_TRAINING: BudgetScope
PORT_UNSPECIFIED: Port
PORT_PROVIDER: Port
PORT_GUARDRAIL: Port
PORT_STORE: Port
PORT_TELEMETRY: Port

class LayerVersion(_message.Message):
    __slots__ = ("number", "digest")
    NUMBER_FIELD_NUMBER: _ClassVar[int]
    DIGEST_FIELD_NUMBER: _ClassVar[int]
    number: int
    digest: str
    def __init__(self, number: _Optional[int] = ..., digest: _Optional[str] = ...) -> None: ...

class RoutingKey(_message.Message):
    __slots__ = ("base_model", "adapter_id")
    BASE_MODEL_FIELD_NUMBER: _ClassVar[int]
    ADAPTER_ID_FIELD_NUMBER: _ClassVar[int]
    base_model: str
    adapter_id: str
    def __init__(self, base_model: _Optional[str] = ..., adapter_id: _Optional[str] = ...) -> None: ...

class Cost(_message.Message):
    __slots__ = ("input_per_1k_micro_usd", "output_per_1k_micro_usd")
    INPUT_PER_1K_MICRO_USD_FIELD_NUMBER: _ClassVar[int]
    OUTPUT_PER_1K_MICRO_USD_FIELD_NUMBER: _ClassVar[int]
    input_per_1k_micro_usd: int
    output_per_1k_micro_usd: int
    def __init__(self, input_per_1k_micro_usd: _Optional[int] = ..., output_per_1k_micro_usd: _Optional[int] = ...) -> None: ...

class Deployment(_message.Message):
    __slots__ = ("id", "key", "provider", "endpoint", "region", "trust_tier", "credential_ref", "weight", "cost", "capabilities")
    ID_FIELD_NUMBER: _ClassVar[int]
    KEY_FIELD_NUMBER: _ClassVar[int]
    PROVIDER_FIELD_NUMBER: _ClassVar[int]
    ENDPOINT_FIELD_NUMBER: _ClassVar[int]
    REGION_FIELD_NUMBER: _ClassVar[int]
    TRUST_TIER_FIELD_NUMBER: _ClassVar[int]
    CREDENTIAL_REF_FIELD_NUMBER: _ClassVar[int]
    WEIGHT_FIELD_NUMBER: _ClassVar[int]
    COST_FIELD_NUMBER: _ClassVar[int]
    CAPABILITIES_FIELD_NUMBER: _ClassVar[int]
    id: str
    key: RoutingKey
    provider: str
    endpoint: str
    region: str
    trust_tier: TrustTier
    credential_ref: str
    weight: int
    cost: Cost
    capabilities: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, id: _Optional[str] = ..., key: _Optional[_Union[RoutingKey, _Mapping]] = ..., provider: _Optional[str] = ..., endpoint: _Optional[str] = ..., region: _Optional[str] = ..., trust_tier: _Optional[_Union[TrustTier, str]] = ..., credential_ref: _Optional[str] = ..., weight: _Optional[int] = ..., cost: _Optional[_Union[Cost, _Mapping]] = ..., capabilities: _Optional[_Iterable[str]] = ...) -> None: ...

class ModelAlias(_message.Message):
    __slots__ = ("name", "targets")
    NAME_FIELD_NUMBER: _ClassVar[int]
    TARGETS_FIELD_NUMBER: _ClassVar[int]
    name: str
    targets: _containers.RepeatedCompositeFieldContainer[RoutingKey]
    def __init__(self, name: _Optional[str] = ..., targets: _Optional[_Iterable[_Union[RoutingKey, _Mapping]]] = ...) -> None: ...

class PluginBinding(_message.Message):
    __slots__ = ("port", "component", "version", "config_ref")
    PORT_FIELD_NUMBER: _ClassVar[int]
    COMPONENT_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    CONFIG_REF_FIELD_NUMBER: _ClassVar[int]
    port: Port
    component: str
    version: str
    config_ref: str
    def __init__(self, port: _Optional[_Union[Port, str]] = ..., component: _Optional[str] = ..., version: _Optional[str] = ..., config_ref: _Optional[str] = ...) -> None: ...

class BudgetState(_message.Message):
    __slots__ = ("id", "scope", "limit_micro_usd", "spent_micro_usd", "hard", "headroom_basis_points")
    ID_FIELD_NUMBER: _ClassVar[int]
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    LIMIT_MICRO_USD_FIELD_NUMBER: _ClassVar[int]
    SPENT_MICRO_USD_FIELD_NUMBER: _ClassVar[int]
    HARD_FIELD_NUMBER: _ClassVar[int]
    HEADROOM_BASIS_POINTS_FIELD_NUMBER: _ClassVar[int]
    id: str
    scope: BudgetScope
    limit_micro_usd: int
    spent_micro_usd: int
    hard: bool
    headroom_basis_points: int
    def __init__(self, id: _Optional[str] = ..., scope: _Optional[_Union[BudgetScope, str]] = ..., limit_micro_usd: _Optional[int] = ..., spent_micro_usd: _Optional[int] = ..., hard: bool = ..., headroom_basis_points: _Optional[int] = ...) -> None: ...

class BudgetRef(_message.Message):
    __slots__ = ("id", "scope")
    ID_FIELD_NUMBER: _ClassVar[int]
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    id: str
    scope: BudgetScope
    def __init__(self, id: _Optional[str] = ..., scope: _Optional[_Union[BudgetScope, str]] = ...) -> None: ...

class RateLimit(_message.Message):
    __slots__ = ("requests_per_minute", "tokens_per_minute", "max_concurrent")
    REQUESTS_PER_MINUTE_FIELD_NUMBER: _ClassVar[int]
    TOKENS_PER_MINUTE_FIELD_NUMBER: _ClassVar[int]
    MAX_CONCURRENT_FIELD_NUMBER: _ClassVar[int]
    requests_per_minute: int
    tokens_per_minute: int
    max_concurrent: int
    def __init__(self, requests_per_minute: _Optional[int] = ..., tokens_per_minute: _Optional[int] = ..., max_concurrent: _Optional[int] = ...) -> None: ...

class Principal(_message.Message):
    __slots__ = ("key_id", "tenant", "org", "team", "user", "app", "roles", "models_allow_all", "models", "budgets", "default_data_class", "min_trust_tier", "max_concurrent", "deprecated", "not_after_unix_ms", "limits")
    KEY_ID_FIELD_NUMBER: _ClassVar[int]
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ORG_FIELD_NUMBER: _ClassVar[int]
    TEAM_FIELD_NUMBER: _ClassVar[int]
    USER_FIELD_NUMBER: _ClassVar[int]
    APP_FIELD_NUMBER: _ClassVar[int]
    ROLES_FIELD_NUMBER: _ClassVar[int]
    MODELS_ALLOW_ALL_FIELD_NUMBER: _ClassVar[int]
    MODELS_FIELD_NUMBER: _ClassVar[int]
    BUDGETS_FIELD_NUMBER: _ClassVar[int]
    DEFAULT_DATA_CLASS_FIELD_NUMBER: _ClassVar[int]
    MIN_TRUST_TIER_FIELD_NUMBER: _ClassVar[int]
    MAX_CONCURRENT_FIELD_NUMBER: _ClassVar[int]
    DEPRECATED_FIELD_NUMBER: _ClassVar[int]
    NOT_AFTER_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    LIMITS_FIELD_NUMBER: _ClassVar[int]
    key_id: str
    tenant: str
    org: str
    team: str
    user: str
    app: str
    roles: _containers.RepeatedScalarFieldContainer[str]
    models_allow_all: bool
    models: _containers.RepeatedScalarFieldContainer[str]
    budgets: _containers.RepeatedCompositeFieldContainer[BudgetRef]
    default_data_class: str
    min_trust_tier: TrustTier
    max_concurrent: int
    deprecated: bool
    not_after_unix_ms: int
    limits: RateLimit
    def __init__(self, key_id: _Optional[str] = ..., tenant: _Optional[str] = ..., org: _Optional[str] = ..., team: _Optional[str] = ..., user: _Optional[str] = ..., app: _Optional[str] = ..., roles: _Optional[_Iterable[str]] = ..., models_allow_all: bool = ..., models: _Optional[_Iterable[str]] = ..., budgets: _Optional[_Iterable[_Union[BudgetRef, _Mapping]]] = ..., default_data_class: _Optional[str] = ..., min_trust_tier: _Optional[_Union[TrustTier, str]] = ..., max_concurrent: _Optional[int] = ..., deprecated: bool = ..., not_after_unix_ms: _Optional[int] = ..., limits: _Optional[_Union[RateLimit, _Mapping]] = ...) -> None: ...

class KeyEntry(_message.Message):
    __slots__ = ("lookup", "key_id")
    LOOKUP_FIELD_NUMBER: _ClassVar[int]
    KEY_ID_FIELD_NUMBER: _ClassVar[int]
    lookup: bytes
    key_id: str
    def __init__(self, lookup: _Optional[bytes] = ..., key_id: _Optional[str] = ...) -> None: ...

class GlobalLayer(_message.Message):
    __slots__ = ("version", "built_at_unix_ms", "deployments", "aliases", "tenant_prefixes", "default_plugins", "policy_bundle_ref")
    class TenantPrefixesEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    VERSION_FIELD_NUMBER: _ClassVar[int]
    BUILT_AT_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    DEPLOYMENTS_FIELD_NUMBER: _ClassVar[int]
    ALIASES_FIELD_NUMBER: _ClassVar[int]
    TENANT_PREFIXES_FIELD_NUMBER: _ClassVar[int]
    DEFAULT_PLUGINS_FIELD_NUMBER: _ClassVar[int]
    POLICY_BUNDLE_REF_FIELD_NUMBER: _ClassVar[int]
    version: LayerVersion
    built_at_unix_ms: int
    deployments: _containers.RepeatedCompositeFieldContainer[Deployment]
    aliases: _containers.RepeatedCompositeFieldContainer[ModelAlias]
    tenant_prefixes: _containers.ScalarMap[str, str]
    default_plugins: _containers.RepeatedCompositeFieldContainer[PluginBinding]
    policy_bundle_ref: str
    def __init__(self, version: _Optional[_Union[LayerVersion, _Mapping]] = ..., built_at_unix_ms: _Optional[int] = ..., deployments: _Optional[_Iterable[_Union[Deployment, _Mapping]]] = ..., aliases: _Optional[_Iterable[_Union[ModelAlias, _Mapping]]] = ..., tenant_prefixes: _Optional[_Mapping[str, str]] = ..., default_plugins: _Optional[_Iterable[_Union[PluginBinding, _Mapping]]] = ..., policy_bundle_ref: _Optional[str] = ...) -> None: ...

class TenantLayer(_message.Message):
    __slots__ = ("tenant", "version", "built_at_unix_ms", "tier", "principals", "keys", "alias_overrides", "budgets", "plugins", "min_trust_tier")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    BUILT_AT_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    TIER_FIELD_NUMBER: _ClassVar[int]
    PRINCIPALS_FIELD_NUMBER: _ClassVar[int]
    KEYS_FIELD_NUMBER: _ClassVar[int]
    ALIAS_OVERRIDES_FIELD_NUMBER: _ClassVar[int]
    BUDGETS_FIELD_NUMBER: _ClassVar[int]
    PLUGINS_FIELD_NUMBER: _ClassVar[int]
    MIN_TRUST_TIER_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    version: LayerVersion
    built_at_unix_ms: int
    tier: str
    principals: _containers.RepeatedCompositeFieldContainer[Principal]
    keys: _containers.RepeatedCompositeFieldContainer[KeyEntry]
    alias_overrides: _containers.RepeatedCompositeFieldContainer[ModelAlias]
    budgets: _containers.RepeatedCompositeFieldContainer[BudgetState]
    plugins: _containers.RepeatedCompositeFieldContainer[PluginBinding]
    min_trust_tier: TrustTier
    def __init__(self, tenant: _Optional[str] = ..., version: _Optional[_Union[LayerVersion, _Mapping]] = ..., built_at_unix_ms: _Optional[int] = ..., tier: _Optional[str] = ..., principals: _Optional[_Iterable[_Union[Principal, _Mapping]]] = ..., keys: _Optional[_Iterable[_Union[KeyEntry, _Mapping]]] = ..., alias_overrides: _Optional[_Iterable[_Union[ModelAlias, _Mapping]]] = ..., budgets: _Optional[_Iterable[_Union[BudgetState, _Mapping]]] = ..., plugins: _Optional[_Iterable[_Union[PluginBinding, _Mapping]]] = ..., min_trust_tier: _Optional[_Union[TrustTier, str]] = ...) -> None: ...

class Snapshot(_message.Message):
    __slots__ = ("global_layer", "tenants")
    GLOBAL_LAYER_FIELD_NUMBER: _ClassVar[int]
    TENANTS_FIELD_NUMBER: _ClassVar[int]
    global_layer: GlobalLayer
    tenants: _containers.RepeatedCompositeFieldContainer[TenantLayer]
    def __init__(self, global_layer: _Optional[_Union[GlobalLayer, _Mapping]] = ..., tenants: _Optional[_Iterable[_Union[TenantLayer, _Mapping]]] = ...) -> None: ...
