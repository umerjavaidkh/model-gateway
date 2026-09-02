from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Iterable as _Iterable, Mapping as _Mapping, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class TokenUsage(_message.Message):
    __slots__ = ("input", "cached_input", "cache_write", "output")
    INPUT_FIELD_NUMBER: _ClassVar[int]
    CACHED_INPUT_FIELD_NUMBER: _ClassVar[int]
    CACHE_WRITE_FIELD_NUMBER: _ClassVar[int]
    OUTPUT_FIELD_NUMBER: _ClassVar[int]
    input: int
    cached_input: int
    cache_write: int
    output: int
    def __init__(self, input: _Optional[int] = ..., cached_input: _Optional[int] = ..., cache_write: _Optional[int] = ..., output: _Optional[int] = ...) -> None: ...

class UsageEvent(_message.Message):
    __slots__ = ("request_id", "timestamp_unix_ms", "tenant", "key_id", "tier", "deployment", "base_model", "adapter_id", "provider", "stream", "usage", "cost_micro_usd", "price_micro_usd", "latency_ms", "time_to_first_byte_ms", "outcome", "snapshot_version", "budget_ids", "shadow", "stages")
    REQUEST_ID_FIELD_NUMBER: _ClassVar[int]
    TIMESTAMP_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    TENANT_FIELD_NUMBER: _ClassVar[int]
    KEY_ID_FIELD_NUMBER: _ClassVar[int]
    TIER_FIELD_NUMBER: _ClassVar[int]
    DEPLOYMENT_FIELD_NUMBER: _ClassVar[int]
    BASE_MODEL_FIELD_NUMBER: _ClassVar[int]
    ADAPTER_ID_FIELD_NUMBER: _ClassVar[int]
    PROVIDER_FIELD_NUMBER: _ClassVar[int]
    STREAM_FIELD_NUMBER: _ClassVar[int]
    USAGE_FIELD_NUMBER: _ClassVar[int]
    COST_MICRO_USD_FIELD_NUMBER: _ClassVar[int]
    PRICE_MICRO_USD_FIELD_NUMBER: _ClassVar[int]
    LATENCY_MS_FIELD_NUMBER: _ClassVar[int]
    TIME_TO_FIRST_BYTE_MS_FIELD_NUMBER: _ClassVar[int]
    OUTCOME_FIELD_NUMBER: _ClassVar[int]
    SNAPSHOT_VERSION_FIELD_NUMBER: _ClassVar[int]
    BUDGET_IDS_FIELD_NUMBER: _ClassVar[int]
    SHADOW_FIELD_NUMBER: _ClassVar[int]
    STAGES_FIELD_NUMBER: _ClassVar[int]
    request_id: str
    timestamp_unix_ms: int
    tenant: str
    key_id: str
    tier: str
    deployment: str
    base_model: str
    adapter_id: str
    provider: str
    stream: bool
    usage: TokenUsage
    cost_micro_usd: int
    price_micro_usd: int
    latency_ms: int
    time_to_first_byte_ms: int
    outcome: str
    snapshot_version: int
    budget_ids: _containers.RepeatedScalarFieldContainer[str]
    shadow: bool
    stages: _containers.RepeatedCompositeFieldContainer[StageTiming]
    def __init__(self, request_id: _Optional[str] = ..., timestamp_unix_ms: _Optional[int] = ..., tenant: _Optional[str] = ..., key_id: _Optional[str] = ..., tier: _Optional[str] = ..., deployment: _Optional[str] = ..., base_model: _Optional[str] = ..., adapter_id: _Optional[str] = ..., provider: _Optional[str] = ..., stream: bool = ..., usage: _Optional[_Union[TokenUsage, _Mapping]] = ..., cost_micro_usd: _Optional[int] = ..., price_micro_usd: _Optional[int] = ..., latency_ms: _Optional[int] = ..., time_to_first_byte_ms: _Optional[int] = ..., outcome: _Optional[str] = ..., snapshot_version: _Optional[int] = ..., budget_ids: _Optional[_Iterable[str]] = ..., shadow: bool = ..., stages: _Optional[_Iterable[_Union[StageTiming, _Mapping]]] = ...) -> None: ...

class StageTiming(_message.Message):
    __slots__ = ("name", "duration_ms", "outcome")
    NAME_FIELD_NUMBER: _ClassVar[int]
    DURATION_MS_FIELD_NUMBER: _ClassVar[int]
    OUTCOME_FIELD_NUMBER: _ClassVar[int]
    name: str
    duration_ms: int
    outcome: str
    def __init__(self, name: _Optional[str] = ..., duration_ms: _Optional[int] = ..., outcome: _Optional[str] = ...) -> None: ...
