"""What the model may answer at each step of an investigation."""

from typing import Literal

from pydantic import BaseModel, Field

SCHEMA_VERSION = "3"

PredicateName = Literal[
    "rollout_recent",
    "failure_onset_after_rollout",
    "prior_revision_was_healthy",
    "oom_killed_new_revision",
    "gitops_managed",
    "config_changed_before_failure",
]


class TargetOut(BaseModel):
    """The incident's target, copied exactly."""

    group: str
    kind: str
    namespace: str
    name: str
    uid: str


class HypothesisOut(BaseModel):
    root_cause_class: str = Field(
        description="A short snake_case class, for example memory_limit_regression."
    )
    summary: str
    supporting: list[str] = Field(description="IDs of evidence supporting the hypothesis.")
    contradicting: list[str] = Field(description="IDs of evidence against it.")
    missing: list[str] = Field(description="Evidence that would settle it but is missing.")
    status: Literal["supported", "refuted", "open"]


class ProposalOut(BaseModel):
    capability: Literal["deployment.rollback-to-revision"]
    target: TargetOut
    revision: int = Field(description="The prior revision to roll back to.")
    expected_effect: str
    rationale: str
    cited_evidence_ids: list[str]
    cited_predicates: list[PredicateName]


class StepDecision(BaseModel):
    kind: Literal["request_evidence", "propose", "conclude_inconclusive"]
    template: str | None = Field(default=None, description="For request_evidence: a template name.")
    params: dict[str, str] = Field(default_factory=dict[str, str])
    summary: str = ""
    hypotheses: list[HypothesisOut] = Field(default_factory=list[HypothesisOut])
    proposal: ProposalOut | None = Field(default=None, description="For propose.")
