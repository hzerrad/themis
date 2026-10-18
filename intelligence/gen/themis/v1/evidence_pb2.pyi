from themis.v1 import common_pb2 as _common_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class CollectEvidenceRequest(_message.Message):
    __slots__ = ("lease_id", "template", "params")
    class ParamsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    LEASE_ID_FIELD_NUMBER: _ClassVar[int]
    TEMPLATE_FIELD_NUMBER: _ClassVar[int]
    PARAMS_FIELD_NUMBER: _ClassVar[int]
    lease_id: str
    template: str
    params: _containers.ScalarMap[str, str]
    def __init__(self, lease_id: _Optional[str] = ..., template: _Optional[str] = ..., params: _Optional[_Mapping[str, str]] = ...) -> None: ...

class CollectEvidenceResponse(_message.Message):
    __slots__ = ("items", "failures", "remaining_requests")
    ITEMS_FIELD_NUMBER: _ClassVar[int]
    FAILURES_FIELD_NUMBER: _ClassVar[int]
    REMAINING_REQUESTS_FIELD_NUMBER: _ClassVar[int]
    items: _containers.RepeatedCompositeFieldContainer[_common_pb2.EvidenceItem]
    failures: _containers.RepeatedCompositeFieldContainer[_common_pb2.CollectionFailure]
    remaining_requests: int
    def __init__(self, items: _Optional[_Iterable[_Union[_common_pb2.EvidenceItem, _Mapping]]] = ..., failures: _Optional[_Iterable[_Union[_common_pb2.CollectionFailure, _Mapping]]] = ..., remaining_requests: _Optional[int] = ...) -> None: ...
