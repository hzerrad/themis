"""What the model may answer at each step of an investigation."""

import json
from typing import Any, Literal

from pydantic import BaseModel, Field

from themis.v1 import investigation_pb2

SCHEMA_VERSION = "5"


class TargetOut(BaseModel):
    """The incident's target, copied exactly."""

    group: str
    kind: str
    namespace: str
    name: str
    uid: str


class HypothesisOut(BaseModel):
    # One of the root causes the core sent: reply_schema lists them.
    root_cause_class: str
    summary: str
    supporting: list[str] = Field(description="IDs of evidence supporting the hypothesis.")
    contradicting: list[str] = Field(description="IDs of evidence against it.")
    missing: list[str] = Field(description="Evidence that would settle it but is missing.")
    status: Literal["supported", "refuted", "open"]


class ProposalOut(BaseModel):
    capability: str = Field(description="One of the allowed fixes, by name.")
    target: TargetOut
    params: dict[str, Any] = Field(description="The fix's parameters, as its schema describes.")
    expected_effect: str
    rationale: str
    cited_evidence_ids: list[str]
    # Checks the core sent, by name: reply_schema lists them.
    cited_predicates: list[str]


class StepDecision(BaseModel):
    kind: Literal["request_evidence", "propose", "conclude_inconclusive"]
    template: str | None = Field(default=None, description="For request_evidence: a template name.")
    params: dict[str, str] = Field(default_factory=dict[str, str])
    summary: str = ""
    hypotheses: list[HypothesisOut] = Field(default_factory=list[HypothesisOut])
    proposal: ProposalOut | None = Field(default=None, description="For propose.")


def checks(job: investigation_pb2.Investigation) -> list[str]:
    """The checks a proposal may cite: those the core ran for the incident."""
    return list(dict.fromkeys(p.name for p in job.predicates))


def root_causes(job: investigation_pb2.Investigation) -> list[str]:
    """The root causes a hypothesis may name: those of the incident's profile."""
    return [c.name for c in job.root_causes]


def reply_schema(job: investigation_pb2.Investigation) -> dict[str, Any]:
    """The step decision's JSON Schema, narrowed to what the core sent: the
    fixes on offer and their parameters, the checks it ran and the root causes
    of the incident's profile, so the model is guided to valid ones. The loop
    still refuses any other, and the core checks every suggestion again."""
    schema = StepDecision.model_json_schema()
    hypothesis = schema["$defs"]["HypothesisOut"]["properties"]
    proposal = schema["$defs"]["ProposalOut"]["properties"]
    if causes := root_causes(job):
        hypothesis["root_cause_class"]["enum"] = causes
    if cited := checks(job):
        proposal["cited_predicates"]["items"]["enum"] = cited
    names = [c.name for c in job.capabilities]
    if not names and not job.candidate_capabilities:
        # No fix is on offer, so proposing isn't a choice the model has.
        kind = schema["properties"]["kind"]
        kind["enum"] = [k for k in kind["enum"] if k != "propose"]
    if names:
        proposal["capability"] = {
            "type": "string",
            "enum": names,
            "description": "One of the allowed fixes, by name.",
        }
    params = [json.loads(c.params_schema) for c in job.capabilities if c.params_schema]
    if len(params) == 1:
        proposal["params"] = params[0]
    elif params:
        proposal["params"] = {"anyOf": params}
    return schema
