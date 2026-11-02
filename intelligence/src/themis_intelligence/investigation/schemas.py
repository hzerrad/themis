"""What the model may answer at each step of an investigation."""

from typing import Literal

from pydantic import BaseModel, Field

SCHEMA_VERSION = "4"

PredicateName = Literal[
    "rollout_recent",
    "failure_onset_after_rollout",
    "prior_revision_was_healthy",
    "oom_killed_new_revision",
    "gitops_managed",
    "config_changed_before_failure",
]

# The root causes a hypothesis may name, and what each means. A closed list
# makes answers comparable; other keeps it honest when none fits.
ROOT_CAUSES: dict[str, str] = {
    "memory_limit_regression": "the new revision's memory limit is too low",
    "bad_configuration": "the new revision's settings are wrong",
    "image_pull_failure": "its image can't be pulled",
    "application_bug": "its code fails, whichever revision runs",
    "configuration_change": "a ConfigMap or Secret changed outside the rollout",
    "dependency_failure": "something it calls is failing",
    "insufficient_capacity": "no room in the cluster for its pods",
    "node_problem": "a node is unhealthy or evicting pods",
    "storage_full": "a disk or volume is full",
    "traffic_spike": "more load than it can take",
    "insufficient_evidence": "the evidence can't tell",
    "other": "none of these: say what in the summary",
}

RootCause = Literal[
    "memory_limit_regression",
    "bad_configuration",
    "image_pull_failure",
    "application_bug",
    "configuration_change",
    "dependency_failure",
    "insufficient_capacity",
    "node_problem",
    "storage_full",
    "traffic_spike",
    "insufficient_evidence",
    "other",
]


class TargetOut(BaseModel):
    """The incident's target, copied exactly."""

    group: str
    kind: str
    namespace: str
    name: str
    uid: str


class HypothesisOut(BaseModel):
    root_cause_class: RootCause
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
