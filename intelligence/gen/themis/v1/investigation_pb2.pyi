import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from themis.v1 import common_pb2 as _common_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class HypothesisStatus(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    HYPOTHESIS_STATUS_UNSPECIFIED: _ClassVar[HypothesisStatus]
    HYPOTHESIS_STATUS_REFUTED: _ClassVar[HypothesisStatus]
    HYPOTHESIS_STATUS_SUPPORTED: _ClassVar[HypothesisStatus]
    HYPOTHESIS_STATUS_OPEN: _ClassVar[HypothesisStatus]

class InvestigationConclusion(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    INVESTIGATION_CONCLUSION_UNSPECIFIED: _ClassVar[InvestigationConclusion]
    INVESTIGATION_CONCLUSION_PROPOSED: _ClassVar[InvestigationConclusion]
    INVESTIGATION_CONCLUSION_INCONCLUSIVE: _ClassVar[InvestigationConclusion]

class InvestigationFailureReason(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    INVESTIGATION_FAILURE_REASON_UNSPECIFIED: _ClassVar[InvestigationFailureReason]
    INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE: _ClassVar[InvestigationFailureReason]
    INVESTIGATION_FAILURE_REASON_INVALID_MODEL_OUTPUT: _ClassVar[InvestigationFailureReason]
    INVESTIGATION_FAILURE_REASON_BUDGET_EXHAUSTED: _ClassVar[InvestigationFailureReason]
    INVESTIGATION_FAILURE_REASON_DEADLINE_EXCEEDED: _ClassVar[InvestigationFailureReason]
    INVESTIGATION_FAILURE_REASON_INTERNAL: _ClassVar[InvestigationFailureReason]

class EvidenceGapCause(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    EVIDENCE_GAP_CAUSE_UNSPECIFIED: _ClassVar[EvidenceGapCause]
    EVIDENCE_GAP_CAUSE_NOT_IN_PROFILE: _ClassVar[EvidenceGapCause]
    EVIDENCE_GAP_CAUSE_DATA_CEILING: _ClassVar[EvidenceGapCause]
    EVIDENCE_GAP_CAUSE_BUDGET_SPENT: _ClassVar[EvidenceGapCause]
HYPOTHESIS_STATUS_UNSPECIFIED: HypothesisStatus
HYPOTHESIS_STATUS_REFUTED: HypothesisStatus
HYPOTHESIS_STATUS_SUPPORTED: HypothesisStatus
HYPOTHESIS_STATUS_OPEN: HypothesisStatus
INVESTIGATION_CONCLUSION_UNSPECIFIED: InvestigationConclusion
INVESTIGATION_CONCLUSION_PROPOSED: InvestigationConclusion
INVESTIGATION_CONCLUSION_INCONCLUSIVE: InvestigationConclusion
INVESTIGATION_FAILURE_REASON_UNSPECIFIED: InvestigationFailureReason
INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE: InvestigationFailureReason
INVESTIGATION_FAILURE_REASON_INVALID_MODEL_OUTPUT: InvestigationFailureReason
INVESTIGATION_FAILURE_REASON_BUDGET_EXHAUSTED: InvestigationFailureReason
INVESTIGATION_FAILURE_REASON_DEADLINE_EXCEEDED: InvestigationFailureReason
INVESTIGATION_FAILURE_REASON_INTERNAL: InvestigationFailureReason
EVIDENCE_GAP_CAUSE_UNSPECIFIED: EvidenceGapCause
EVIDENCE_GAP_CAUSE_NOT_IN_PROFILE: EvidenceGapCause
EVIDENCE_GAP_CAUSE_DATA_CEILING: EvidenceGapCause
EVIDENCE_GAP_CAUSE_BUDGET_SPENT: EvidenceGapCause

class LeaseInvestigationRequest(_message.Message):
    __slots__ = ("worker_id",)
    WORKER_ID_FIELD_NUMBER: _ClassVar[int]
    worker_id: str
    def __init__(self, worker_id: _Optional[str] = ...) -> None: ...

class LeaseInvestigationResponse(_message.Message):
    __slots__ = ("investigation",)
    INVESTIGATION_FIELD_NUMBER: _ClassVar[int]
    investigation: Investigation
    def __init__(self, investigation: _Optional[_Union[Investigation, _Mapping]] = ...) -> None: ...

class RenewLeaseRequest(_message.Message):
    __slots__ = ("lease_id",)
    LEASE_ID_FIELD_NUMBER: _ClassVar[int]
    lease_id: str
    def __init__(self, lease_id: _Optional[str] = ...) -> None: ...

class RenewLeaseResponse(_message.Message):
    __slots__ = ("lease_deadline",)
    LEASE_DEADLINE_FIELD_NUMBER: _ClassVar[int]
    lease_deadline: _timestamp_pb2.Timestamp
    def __init__(self, lease_deadline: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class CompleteInvestigationRequest(_message.Message):
    __slots__ = ("lease_id", "conclusion", "summary", "hypotheses", "usage", "provenance", "gaps")
    LEASE_ID_FIELD_NUMBER: _ClassVar[int]
    CONCLUSION_FIELD_NUMBER: _ClassVar[int]
    SUMMARY_FIELD_NUMBER: _ClassVar[int]
    HYPOTHESES_FIELD_NUMBER: _ClassVar[int]
    USAGE_FIELD_NUMBER: _ClassVar[int]
    PROVENANCE_FIELD_NUMBER: _ClassVar[int]
    GAPS_FIELD_NUMBER: _ClassVar[int]
    lease_id: str
    conclusion: InvestigationConclusion
    summary: str
    hypotheses: _containers.RepeatedCompositeFieldContainer[Hypothesis]
    usage: Usage
    provenance: _common_pb2.Provenance
    gaps: _containers.RepeatedCompositeFieldContainer[EvidenceGap]
    def __init__(self, lease_id: _Optional[str] = ..., conclusion: _Optional[_Union[InvestigationConclusion, str]] = ..., summary: _Optional[str] = ..., hypotheses: _Optional[_Iterable[_Union[Hypothesis, _Mapping]]] = ..., usage: _Optional[_Union[Usage, _Mapping]] = ..., provenance: _Optional[_Union[_common_pb2.Provenance, _Mapping]] = ..., gaps: _Optional[_Iterable[_Union[EvidenceGap, _Mapping]]] = ...) -> None: ...

class CompleteInvestigationResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class FailInvestigationRequest(_message.Message):
    __slots__ = ("lease_id", "reason", "message", "gaps")
    LEASE_ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    GAPS_FIELD_NUMBER: _ClassVar[int]
    lease_id: str
    reason: InvestigationFailureReason
    message: str
    gaps: _containers.RepeatedCompositeFieldContainer[EvidenceGap]
    def __init__(self, lease_id: _Optional[str] = ..., reason: _Optional[_Union[InvestigationFailureReason, str]] = ..., message: _Optional[str] = ..., gaps: _Optional[_Iterable[_Union[EvidenceGap, _Mapping]]] = ...) -> None: ...

class EvidenceGap(_message.Message):
    __slots__ = ("template", "cause", "detail")
    TEMPLATE_FIELD_NUMBER: _ClassVar[int]
    CAUSE_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    template: str
    cause: EvidenceGapCause
    detail: str
    def __init__(self, template: _Optional[str] = ..., cause: _Optional[_Union[EvidenceGapCause, str]] = ..., detail: _Optional[str] = ...) -> None: ...

class FailInvestigationResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class Investigation(_message.Message):
    __slots__ = ("lease_id", "lease_deadline", "incident_id", "profile", "target", "initial_evidence", "initial_failures", "predicates", "allowed_templates", "candidate_capabilities", "budget")
    LEASE_ID_FIELD_NUMBER: _ClassVar[int]
    LEASE_DEADLINE_FIELD_NUMBER: _ClassVar[int]
    INCIDENT_ID_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    TARGET_FIELD_NUMBER: _ClassVar[int]
    INITIAL_EVIDENCE_FIELD_NUMBER: _ClassVar[int]
    INITIAL_FAILURES_FIELD_NUMBER: _ClassVar[int]
    PREDICATES_FIELD_NUMBER: _ClassVar[int]
    ALLOWED_TEMPLATES_FIELD_NUMBER: _ClassVar[int]
    CANDIDATE_CAPABILITIES_FIELD_NUMBER: _ClassVar[int]
    BUDGET_FIELD_NUMBER: _ClassVar[int]
    lease_id: str
    lease_deadline: _timestamp_pb2.Timestamp
    incident_id: str
    profile: str
    target: _common_pb2.TargetRef
    initial_evidence: _containers.RepeatedCompositeFieldContainer[_common_pb2.EvidenceItem]
    initial_failures: _containers.RepeatedCompositeFieldContainer[_common_pb2.CollectionFailure]
    predicates: _containers.RepeatedCompositeFieldContainer[_common_pb2.PredicateResult]
    allowed_templates: _containers.RepeatedCompositeFieldContainer[EvidenceTemplate]
    candidate_capabilities: _containers.RepeatedScalarFieldContainer[str]
    budget: _common_pb2.Budget
    def __init__(self, lease_id: _Optional[str] = ..., lease_deadline: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., incident_id: _Optional[str] = ..., profile: _Optional[str] = ..., target: _Optional[_Union[_common_pb2.TargetRef, _Mapping]] = ..., initial_evidence: _Optional[_Iterable[_Union[_common_pb2.EvidenceItem, _Mapping]]] = ..., initial_failures: _Optional[_Iterable[_Union[_common_pb2.CollectionFailure, _Mapping]]] = ..., predicates: _Optional[_Iterable[_Union[_common_pb2.PredicateResult, _Mapping]]] = ..., allowed_templates: _Optional[_Iterable[_Union[EvidenceTemplate, _Mapping]]] = ..., candidate_capabilities: _Optional[_Iterable[str]] = ..., budget: _Optional[_Union[_common_pb2.Budget, _Mapping]] = ...) -> None: ...

class Hypothesis(_message.Message):
    __slots__ = ("root_cause_class", "summary", "supporting_evidence_ids", "contradicting_evidence_ids", "missing", "status")
    ROOT_CAUSE_CLASS_FIELD_NUMBER: _ClassVar[int]
    SUMMARY_FIELD_NUMBER: _ClassVar[int]
    SUPPORTING_EVIDENCE_IDS_FIELD_NUMBER: _ClassVar[int]
    CONTRADICTING_EVIDENCE_IDS_FIELD_NUMBER: _ClassVar[int]
    MISSING_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    root_cause_class: str
    summary: str
    supporting_evidence_ids: _containers.RepeatedScalarFieldContainer[str]
    contradicting_evidence_ids: _containers.RepeatedScalarFieldContainer[str]
    missing: _containers.RepeatedScalarFieldContainer[str]
    status: HypothesisStatus
    def __init__(self, root_cause_class: _Optional[str] = ..., summary: _Optional[str] = ..., supporting_evidence_ids: _Optional[_Iterable[str]] = ..., contradicting_evidence_ids: _Optional[_Iterable[str]] = ..., missing: _Optional[_Iterable[str]] = ..., status: _Optional[_Union[HypothesisStatus, str]] = ...) -> None: ...

class EvidenceTemplate(_message.Message):
    __slots__ = ("name", "description", "params")
    NAME_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    PARAMS_FIELD_NUMBER: _ClassVar[int]
    name: str
    description: str
    params: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, name: _Optional[str] = ..., description: _Optional[str] = ..., params: _Optional[_Iterable[str]] = ...) -> None: ...

class Usage(_message.Message):
    __slots__ = ("steps_used", "input_tokens", "output_tokens")
    STEPS_USED_FIELD_NUMBER: _ClassVar[int]
    INPUT_TOKENS_FIELD_NUMBER: _ClassVar[int]
    OUTPUT_TOKENS_FIELD_NUMBER: _ClassVar[int]
    steps_used: int
    input_tokens: int
    output_tokens: int
    def __init__(self, steps_used: _Optional[int] = ..., input_tokens: _Optional[int] = ..., output_tokens: _Optional[int] = ...) -> None: ...
