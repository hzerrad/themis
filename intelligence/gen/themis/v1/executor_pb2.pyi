from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ExecuteActionStatus(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    EXECUTE_ACTION_STATUS_UNSPECIFIED: _ClassVar[ExecuteActionStatus]
    EXECUTE_ACTION_STATUS_SUCCEEDED: _ClassVar[ExecuteActionStatus]
    EXECUTE_ACTION_STATUS_FAILED: _ClassVar[ExecuteActionStatus]
    EXECUTE_ACTION_STATUS_AMBIGUOUS: _ClassVar[ExecuteActionStatus]
    EXECUTE_ACTION_STATUS_STALE: _ClassVar[ExecuteActionStatus]
EXECUTE_ACTION_STATUS_UNSPECIFIED: ExecuteActionStatus
EXECUTE_ACTION_STATUS_SUCCEEDED: ExecuteActionStatus
EXECUTE_ACTION_STATUS_FAILED: ExecuteActionStatus
EXECUTE_ACTION_STATUS_AMBIGUOUS: ExecuteActionStatus
EXECUTE_ACTION_STATUS_STALE: ExecuteActionStatus

class ExecuteActionRequest(_message.Message):
    __slots__ = ("action_id",)
    ACTION_ID_FIELD_NUMBER: _ClassVar[int]
    action_id: str
    def __init__(self, action_id: _Optional[str] = ...) -> None: ...

class ExecuteActionResponse(_message.Message):
    __slots__ = ("status", "message", "post_generation")
    STATUS_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    POST_GENERATION_FIELD_NUMBER: _ClassVar[int]
    status: ExecuteActionStatus
    message: str
    post_generation: int
    def __init__(self, status: _Optional[_Union[ExecuteActionStatus, str]] = ..., message: _Optional[str] = ..., post_generation: _Optional[int] = ...) -> None: ...
