from themis.v1 import common_pb2 as _common_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ProposalStatus(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    PROPOSAL_STATUS_UNSPECIFIED: _ClassVar[ProposalStatus]
    PROPOSAL_STATUS_ACCEPTED: _ClassVar[ProposalStatus]
    PROPOSAL_STATUS_REJECTED: _ClassVar[ProposalStatus]

class RejectionReason(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    REJECTION_REASON_UNSPECIFIED: _ClassVar[RejectionReason]
    REJECTION_REASON_TARGET_MISMATCH: _ClassVar[RejectionReason]
    REJECTION_REASON_CAPABILITY_NOT_ALLOWED: _ClassVar[RejectionReason]
    REJECTION_REASON_INVALID_PARAMS: _ClassVar[RejectionReason]
    REJECTION_REASON_UNKNOWN_EVIDENCE: _ClassVar[RejectionReason]
    REJECTION_REASON_UNKNOWN_PREDICATE: _ClassVar[RejectionReason]
PROPOSAL_STATUS_UNSPECIFIED: ProposalStatus
PROPOSAL_STATUS_ACCEPTED: ProposalStatus
PROPOSAL_STATUS_REJECTED: ProposalStatus
REJECTION_REASON_UNSPECIFIED: RejectionReason
REJECTION_REASON_TARGET_MISMATCH: RejectionReason
REJECTION_REASON_CAPABILITY_NOT_ALLOWED: RejectionReason
REJECTION_REASON_INVALID_PARAMS: RejectionReason
REJECTION_REASON_UNKNOWN_EVIDENCE: RejectionReason
REJECTION_REASON_UNKNOWN_PREDICATE: RejectionReason

class SubmitProposalRequest(_message.Message):
    __slots__ = ("lease_id", "client_proposal_id", "target", "expected_effect", "rationale", "cited_evidence_ids", "cited_predicates", "provenance", "rollback_to_revision")
    LEASE_ID_FIELD_NUMBER: _ClassVar[int]
    CLIENT_PROPOSAL_ID_FIELD_NUMBER: _ClassVar[int]
    TARGET_FIELD_NUMBER: _ClassVar[int]
    EXPECTED_EFFECT_FIELD_NUMBER: _ClassVar[int]
    RATIONALE_FIELD_NUMBER: _ClassVar[int]
    CITED_EVIDENCE_IDS_FIELD_NUMBER: _ClassVar[int]
    CITED_PREDICATES_FIELD_NUMBER: _ClassVar[int]
    PROVENANCE_FIELD_NUMBER: _ClassVar[int]
    ROLLBACK_TO_REVISION_FIELD_NUMBER: _ClassVar[int]
    lease_id: str
    client_proposal_id: str
    target: _common_pb2.TargetRef
    expected_effect: str
    rationale: str
    cited_evidence_ids: _containers.RepeatedScalarFieldContainer[str]
    cited_predicates: _containers.RepeatedScalarFieldContainer[str]
    provenance: _common_pb2.Provenance
    rollback_to_revision: RollbackToRevisionParams
    def __init__(self, lease_id: _Optional[str] = ..., client_proposal_id: _Optional[str] = ..., target: _Optional[_Union[_common_pb2.TargetRef, _Mapping]] = ..., expected_effect: _Optional[str] = ..., rationale: _Optional[str] = ..., cited_evidence_ids: _Optional[_Iterable[str]] = ..., cited_predicates: _Optional[_Iterable[str]] = ..., provenance: _Optional[_Union[_common_pb2.Provenance, _Mapping]] = ..., rollback_to_revision: _Optional[_Union[RollbackToRevisionParams, _Mapping]] = ...) -> None: ...

class RollbackToRevisionParams(_message.Message):
    __slots__ = ("revision",)
    REVISION_FIELD_NUMBER: _ClassVar[int]
    revision: int
    def __init__(self, revision: _Optional[int] = ...) -> None: ...

class SubmitProposalResponse(_message.Message):
    __slots__ = ("proposal_id", "status", "rejection_reason", "message")
    PROPOSAL_ID_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    REJECTION_REASON_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    proposal_id: str
    status: ProposalStatus
    rejection_reason: RejectionReason
    message: str
    def __init__(self, proposal_id: _Optional[str] = ..., status: _Optional[_Union[ProposalStatus, str]] = ..., rejection_reason: _Optional[_Union[RejectionReason, str]] = ..., message: _Optional[str] = ...) -> None: ...
