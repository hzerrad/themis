"""Scenario bundles, as internal/evaluation writes them, and the job each one becomes."""

from pathlib import Path
from typing import Literal

from pydantic import BaseModel, Field

from themis.v1 import common_pb2, investigation_pb2

# Every scenario gets the same IDs and profile, so they can't hint at the answer.
INCIDENT_ID = "inc-eval"
LEASE_ID = "lease-eval"
PROFILE = "rollout-regression"

_CLASSIFICATIONS = {
    "public": common_pb2.DATA_CLASSIFICATION_PUBLIC,
    "internal": common_pb2.DATA_CLASSIFICATION_INTERNAL,
    "sensitive": common_pb2.DATA_CLASSIFICATION_SENSITIVE,
}

_TRUTHS = {
    "true": common_pb2.TRUTH_TRUE,
    "false": common_pb2.TRUTH_FALSE,
    "unknown": common_pb2.TRUTH_UNKNOWN,
}


class Truth(BaseModel):
    root_causes: list[str]
    decision: Literal["propose", "abstain", "either"]
    acceptable_revisions: list[int] = Field(default_factory=list[int])
    rollback_is_harmful: bool = False
    required_citations: list[str] = Field(default_factory=list[str])
    predicates: dict[str, str] = Field(default_factory=dict[str, str])


class Target(BaseModel):
    Group: str
    Kind: str
    Namespace: str
    Name: str
    UID: str


class Template(BaseModel):
    name: str
    description: str
    params: list[str] = Field(default_factory=list[str])


class Item(BaseModel):
    id: str
    template: str
    data: str
    truncated: bool = False
    original_bytes: int = 0
    redactions: int = 0
    classification: str


class Check(BaseModel):
    name: str
    version: str
    value: str
    reason: str
    evidence_ids: list[str] = Field(default_factory=list[str])


class Bundle(BaseModel):
    scenario: str
    version: int
    difficulty: str
    incident_class: str
    truth: Truth
    target: Target
    capabilities: list[str]
    max_steps: int
    max_evidence_requests: int
    initial: list[Item]
    requestable: dict[str, Item]
    templates: list[Template]
    predicates: list[Check]


def load_bundles(root: Path, names: list[str] | None = None) -> list[Bundle]:
    """Reads every scenario under root, or only those named, in name order."""
    dirs = sorted(p.parent for p in root.glob("*/bundle.json"))
    if names:
        known = {d.name for d in dirs}
        missing = sorted(set(names) - known)
        if missing:
            raise ValueError(f"no such scenario: {', '.join(missing)}")
        dirs = [d for d in dirs if d.name in names]
    return [Bundle.model_validate_json((d / "bundle.json").read_bytes()) for d in dirs]


def target_proto(b: Bundle) -> common_pb2.TargetRef:
    t = b.target
    return common_pb2.TargetRef(
        group=t.Group, kind=t.Kind, namespace=t.Namespace, name=t.Name, uid=t.UID
    )


def item_proto(b: Bundle, item: Item) -> common_pb2.EvidenceItem:
    return common_pb2.EvidenceItem(
        id=item.id,
        template=item.template,
        target=target_proto(b),
        content_type="application/json",
        data=item.data.encode(),
        truncated=item.truncated,
        original_bytes=item.original_bytes,
        redactions=item.redactions,
        # Anything unrecognised is treated as the most sensitive, as in the loop.
        classification=_CLASSIFICATIONS.get(
            item.classification, common_pb2.DATA_CLASSIFICATION_SENSITIVE
        ),
    )


def job_of(b: Bundle) -> investigation_pb2.Investigation:
    """The job the core would lease for this scenario. Truth stays out of it."""
    return investigation_pb2.Investigation(
        lease_id=LEASE_ID,
        incident_id=INCIDENT_ID,
        profile=PROFILE,
        target=target_proto(b),
        candidate_capabilities=b.capabilities,
        initial_evidence=[item_proto(b, i) for i in b.initial],
        predicates=[
            common_pb2.PredicateResult(
                name=c.name,
                value=_TRUTHS.get(c.value, common_pb2.TRUTH_UNKNOWN),
                reason=c.reason,
                evidence_ids=c.evidence_ids,
            )
            for c in b.predicates
        ],
        allowed_templates=[
            investigation_pb2.EvidenceTemplate(
                name=t.name, description=t.description, params=t.params
            )
            for t in b.templates
        ],
    )
