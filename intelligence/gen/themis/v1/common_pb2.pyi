import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class Truth(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    TRUTH_UNSPECIFIED: _ClassVar[Truth]
    TRUTH_TRUE: _ClassVar[Truth]
    TRUTH_FALSE: _ClassVar[Truth]
    TRUTH_UNKNOWN: _ClassVar[Truth]

class DataClassification(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    DATA_CLASSIFICATION_UNSPECIFIED: _ClassVar[DataClassification]
    DATA_CLASSIFICATION_PUBLIC: _ClassVar[DataClassification]
    DATA_CLASSIFICATION_INTERNAL: _ClassVar[DataClassification]
    DATA_CLASSIFICATION_SENSITIVE: _ClassVar[DataClassification]

class CollectionFailureReason(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    COLLECTION_FAILURE_REASON_UNSPECIFIED: _ClassVar[CollectionFailureReason]
    COLLECTION_FAILURE_REASON_NOT_FOUND: _ClassVar[CollectionFailureReason]
    COLLECTION_FAILURE_REASON_NOT_AVAILABLE: _ClassVar[CollectionFailureReason]
    COLLECTION_FAILURE_REASON_FORBIDDEN: _ClassVar[CollectionFailureReason]
    COLLECTION_FAILURE_REASON_TIMEOUT: _ClassVar[CollectionFailureReason]
TRUTH_UNSPECIFIED: Truth
TRUTH_TRUE: Truth
TRUTH_FALSE: Truth
TRUTH_UNKNOWN: Truth
DATA_CLASSIFICATION_UNSPECIFIED: DataClassification
DATA_CLASSIFICATION_PUBLIC: DataClassification
DATA_CLASSIFICATION_INTERNAL: DataClassification
DATA_CLASSIFICATION_SENSITIVE: DataClassification
COLLECTION_FAILURE_REASON_UNSPECIFIED: CollectionFailureReason
COLLECTION_FAILURE_REASON_NOT_FOUND: CollectionFailureReason
COLLECTION_FAILURE_REASON_NOT_AVAILABLE: CollectionFailureReason
COLLECTION_FAILURE_REASON_FORBIDDEN: CollectionFailureReason
COLLECTION_FAILURE_REASON_TIMEOUT: CollectionFailureReason

class TargetRef(_message.Message):
    __slots__ = ("kind", "namespace", "name", "uid", "group")
    KIND_FIELD_NUMBER: _ClassVar[int]
    NAMESPACE_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    UID_FIELD_NUMBER: _ClassVar[int]
    GROUP_FIELD_NUMBER: _ClassVar[int]
    kind: str
    namespace: str
    name: str
    uid: str
    group: str
    def __init__(self, kind: _Optional[str] = ..., namespace: _Optional[str] = ..., name: _Optional[str] = ..., uid: _Optional[str] = ..., group: _Optional[str] = ...) -> None: ...

class PredicateResult(_message.Message):
    __slots__ = ("name", "value", "reason", "evidence_ids", "predicate_version")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    EVIDENCE_IDS_FIELD_NUMBER: _ClassVar[int]
    PREDICATE_VERSION_FIELD_NUMBER: _ClassVar[int]
    name: str
    value: Truth
    reason: str
    evidence_ids: _containers.RepeatedScalarFieldContainer[str]
    predicate_version: str
    def __init__(self, name: _Optional[str] = ..., value: _Optional[_Union[Truth, str]] = ..., reason: _Optional[str] = ..., evidence_ids: _Optional[_Iterable[str]] = ..., predicate_version: _Optional[str] = ...) -> None: ...

class EvidenceItem(_message.Message):
    __slots__ = ("id", "template", "target", "content_type", "data", "truncated", "original_bytes", "redactions", "collected_at", "classification", "template_version", "attributes", "window_start", "window_end", "content_digest")
    class AttributesEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    ID_FIELD_NUMBER: _ClassVar[int]
    TEMPLATE_FIELD_NUMBER: _ClassVar[int]
    TARGET_FIELD_NUMBER: _ClassVar[int]
    CONTENT_TYPE_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    TRUNCATED_FIELD_NUMBER: _ClassVar[int]
    ORIGINAL_BYTES_FIELD_NUMBER: _ClassVar[int]
    REDACTIONS_FIELD_NUMBER: _ClassVar[int]
    COLLECTED_AT_FIELD_NUMBER: _ClassVar[int]
    CLASSIFICATION_FIELD_NUMBER: _ClassVar[int]
    TEMPLATE_VERSION_FIELD_NUMBER: _ClassVar[int]
    ATTRIBUTES_FIELD_NUMBER: _ClassVar[int]
    WINDOW_START_FIELD_NUMBER: _ClassVar[int]
    WINDOW_END_FIELD_NUMBER: _ClassVar[int]
    CONTENT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    id: str
    template: str
    target: TargetRef
    content_type: str
    data: bytes
    truncated: bool
    original_bytes: int
    redactions: int
    collected_at: _timestamp_pb2.Timestamp
    classification: DataClassification
    template_version: str
    attributes: _containers.ScalarMap[str, str]
    window_start: _timestamp_pb2.Timestamp
    window_end: _timestamp_pb2.Timestamp
    content_digest: str
    def __init__(self, id: _Optional[str] = ..., template: _Optional[str] = ..., target: _Optional[_Union[TargetRef, _Mapping]] = ..., content_type: _Optional[str] = ..., data: _Optional[bytes] = ..., truncated: _Optional[bool] = ..., original_bytes: _Optional[int] = ..., redactions: _Optional[int] = ..., collected_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., classification: _Optional[_Union[DataClassification, str]] = ..., template_version: _Optional[str] = ..., attributes: _Optional[_Mapping[str, str]] = ..., window_start: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., window_end: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., content_digest: _Optional[str] = ...) -> None: ...

class Budget(_message.Message):
    __slots__ = ("max_steps", "max_input_tokens", "max_output_tokens", "deadline")
    MAX_STEPS_FIELD_NUMBER: _ClassVar[int]
    MAX_INPUT_TOKENS_FIELD_NUMBER: _ClassVar[int]
    MAX_OUTPUT_TOKENS_FIELD_NUMBER: _ClassVar[int]
    DEADLINE_FIELD_NUMBER: _ClassVar[int]
    max_steps: int
    max_input_tokens: int
    max_output_tokens: int
    deadline: _timestamp_pb2.Timestamp
    def __init__(self, max_steps: _Optional[int] = ..., max_input_tokens: _Optional[int] = ..., max_output_tokens: _Optional[int] = ..., deadline: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class Provenance(_message.Message):
    __slots__ = ("source", "model", "prompt_version", "route")
    SOURCE_FIELD_NUMBER: _ClassVar[int]
    MODEL_FIELD_NUMBER: _ClassVar[int]
    PROMPT_VERSION_FIELD_NUMBER: _ClassVar[int]
    ROUTE_FIELD_NUMBER: _ClassVar[int]
    source: str
    model: str
    prompt_version: str
    route: str
    def __init__(self, source: _Optional[str] = ..., model: _Optional[str] = ..., prompt_version: _Optional[str] = ..., route: _Optional[str] = ...) -> None: ...

class CollectionFailure(_message.Message):
    __slots__ = ("target", "reason", "message")
    TARGET_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    target: TargetRef
    reason: CollectionFailureReason
    message: str
    def __init__(self, target: _Optional[_Union[TargetRef, _Mapping]] = ..., reason: _Optional[_Union[CollectionFailureReason, str]] = ..., message: _Optional[str] = ...) -> None: ...
