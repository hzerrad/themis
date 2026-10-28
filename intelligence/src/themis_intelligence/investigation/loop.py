"""The investigation loop: an explicit state machine over model decisions."""

import json
from dataclasses import dataclass, field
from importlib import resources
from typing import Literal, Protocol

import grpc
from pydantic import BaseModel

from themis.v1 import common_pb2, evidence_pb2, investigation_pb2, proposal_pb2
from themis_intelligence.investigation.schemas import (
    SCHEMA_VERSION,
    HypothesisOut,
    ProposalOut,
    StepDecision,
)
from themis_intelligence.platform.contract import (
    CLASSIFICATION_RANK,
    Classification,
    Message,
    ModelRequest,
    ModelResult,
    Usage,
)

PROMPT_VERSION = "rollout_v1"
MAX_OUTPUT_TOKENS = 2048
STEP_DEADLINE_S = 120.0

FailureReason = Literal[
    "model_unavailable", "invalid_model_output", "budget_exhausted", "deadline_exceeded"
]


class InvestigationError(Exception):
    def __init__(self, reason: FailureReason, message: str) -> None:
        super().__init__(f"{reason}: {message}")
        self.reason = reason


class Model(Protocol):
    async def complete[T: BaseModel](
        self, req: ModelRequest, schema: type[T]
    ) -> ModelResult[T]: ...


class Core(Protocol):
    async def collect_evidence(
        self, req: evidence_pb2.CollectEvidenceRequest
    ) -> evidence_pb2.CollectEvidenceResponse: ...

    async def submit_proposal(
        self, req: proposal_pb2.SubmitProposalRequest
    ) -> proposal_pb2.SubmitProposalResponse: ...


@dataclass
class InvestigationResult:
    conclusion: Literal["proposed", "inconclusive"]
    summary: str
    hypotheses: list[investigation_pb2.Hypothesis]
    usage: investigation_pb2.Usage
    provenance: common_pb2.Provenance
    invalid_steps: int = 0
    proposal_ids: list[str] = field(default_factory=list[str])


_CLASSIFICATIONS: dict[int, Classification] = {
    common_pb2.DATA_CLASSIFICATION_PUBLIC: "public",
    common_pb2.DATA_CLASSIFICATION_INTERNAL: "internal",
}

_STATUSES = {
    "supported": investigation_pb2.HYPOTHESIS_STATUS_SUPPORTED,
    "refuted": investigation_pb2.HYPOTHESIS_STATUS_REFUTED,
    "open": investigation_pb2.HYPOTHESIS_STATUS_OPEN,
}

_TRUTHS = {common_pb2.TRUTH_TRUE: "true", common_pb2.TRUTH_FALSE: "false"}


def higher(a: Classification, b: Classification) -> Classification:
    return a if CLASSIFICATION_RANK[a] >= CLASSIFICATION_RANK[b] else b


def classification(item: common_pb2.EvidenceItem) -> Classification:
    # Anything unrecognised is treated as the most sensitive.
    return _CLASSIFICATIONS.get(item.classification, "sensitive")


def fence(text: str) -> str:
    """Escapes text so it cannot close or forge an evidence tag."""
    return text.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def fence_json(data: bytes) -> str:
    """Re-encodes JSON with angle brackets escaped: same content, no tags."""
    try:
        text = json.dumps(json.loads(data), ensure_ascii=False)
    except ValueError:
        return fence(data.decode("utf-8", errors="replace"))
    return text.replace("<", "\\u003c").replace(">", "\\u003e")


def render_evidence(item: common_pb2.EvidenceItem) -> str:
    flag = ' truncated="true"' if item.truncated else ""
    return (
        f'<evidence id="{fence(item.id)}" template="{fence(item.template)}"{flag}>\n'
        f"{fence_json(item.data)}\n</evidence>"
    )


def system_prompt(job: investigation_pb2.Investigation, max_steps: int) -> str:
    text = resources.files(__package__).joinpath("prompts", f"{PROMPT_VERSION}.md").read_text()
    templates = "\n".join(
        f"  - {t.name}: {t.description}" + (f" Params: {', '.join(t.params)}." if t.params else "")
        for t in job.allowed_templates
    )
    return (
        text.replace("{templates}", templates)
        .replace("{capabilities}", ", ".join(job.candidate_capabilities) or "none")
        .replace("{max_steps}", str(max_steps))
    )


def incident_prompt(job: investigation_pb2.Investigation) -> str:
    t = job.target
    predicates = "\n".join(
        f"- {p.name} = {_TRUTHS.get(p.value, 'unknown')}: {p.reason}"
        + (f" (evidence {', '.join(p.evidence_ids)})" if p.evidence_ids else "")
        for p in job.predicates
    )
    evidence = "\n\n".join(render_evidence(i) for i in job.initial_evidence)
    target = json.dumps(
        {"group": t.group, "kind": t.kind, "namespace": t.namespace, "name": t.name, "uid": t.uid}
    )
    return (
        f"Incident {job.incident_id}, profile {job.profile}.\n"
        f"Target: {target}\n\n"
        f"Predicates:\n{predicates or '- none'}\n\n"
        f"Evidence:\n\n{evidence}"
    )


def hypothesis_to_proto(h: HypothesisOut) -> investigation_pb2.Hypothesis:
    return investigation_pb2.Hypothesis(
        root_cause_class=h.root_cause_class,
        summary=h.summary,
        supporting_evidence_ids=h.supporting,
        contradicting_evidence_ids=h.contradicting,
        missing=h.missing,
        status=_STATUSES[h.status],
    )


def proposal_request(
    job: investigation_pb2.Investigation,
    p: ProposalOut,
    step: int,
    provenance: common_pb2.Provenance,
) -> proposal_pb2.SubmitProposalRequest:
    # The target is forwarded as the model wrote it: the core judges it.
    return proposal_pb2.SubmitProposalRequest(
        lease_id=job.lease_id,
        client_proposal_id=f"{job.lease_id}/{step}",
        target=common_pb2.TargetRef(
            group=p.target.group,
            kind=p.target.kind,
            namespace=p.target.namespace,
            name=p.target.name,
            uid=p.target.uid,
        ),
        expected_effect=p.expected_effect,
        rationale=p.rationale,
        cited_evidence_ids=p.cited_evidence_ids,
        cited_predicates=p.cited_predicates,
        provenance=provenance,
        rollback_to_revision=proposal_pb2.RollbackToRevisionParams(revision=p.revision),
    )


async def investigate(
    job: investigation_pb2.Investigation, core: Core, model: Model, max_steps: int
) -> InvestigationResult:
    messages = [
        Message(role="system", content=system_prompt(job, max_steps)),
        Message(role="user", content=incident_prompt(job)),
    ]
    level: Classification = "internal"
    for i in job.initial_evidence:
        level = higher(level, classification(i))
    allowed = {t.name for t in job.allowed_templates}
    usage = Usage()
    provenance = common_pb2.Provenance(source="themis-intelligence", prompt_version=PROMPT_VERSION)
    invalid = 0
    invalid_in_a_row = 0
    hypotheses: list[HypothesisOut] = []
    summary = ""

    def tell(content: str) -> None:
        messages.append(Message(role="user", content=content))

    def done(
        conclusion: Literal["proposed", "inconclusive"], steps: int, ids: list[str]
    ) -> InvestigationResult:
        return InvestigationResult(
            conclusion=conclusion,
            summary=summary,
            hypotheses=[hypothesis_to_proto(h) for h in hypotheses],
            usage=investigation_pb2.Usage(
                steps_used=steps, input_tokens=usage.input_tokens, output_tokens=usage.output_tokens
            ),
            provenance=provenance,
            invalid_steps=invalid,
            proposal_ids=ids,
        )

    for step in range(1, max_steps + 1):
        result = await model.complete(
            ModelRequest(
                task_class="investigation_step",
                incident_id=job.incident_id,
                trace_id=job.incident_id,
                schema_version=SCHEMA_VERSION,
                data_classification=level,
                prompt_version=PROMPT_VERSION,
                messages=messages,
                deadline_s=STEP_DEADLINE_S,
                max_output_tokens=MAX_OUTPUT_TOKENS,
            ),
            StepDecision,
        )
        usage = usage + result.usage
        provenance.model, provenance.route = result.model, result.route
        if result.error is not None:
            match result.error.kind:
                case "validation_failed":
                    invalid += 1
                    invalid_in_a_row += 1
                    if invalid_in_a_row >= 2:
                        raise InvestigationError("invalid_model_output", result.error.message)
                    tell("Your last reply was not a valid decision. Reply with one JSON decision.")
                    continue
                case "budget_exceeded":
                    raise InvestigationError("budget_exhausted", result.error.message)
                case "deadline_exceeded":
                    raise InvestigationError("deadline_exceeded", result.error.message)
                case "unavailable" | "ineligible":
                    raise InvestigationError("model_unavailable", result.error.message)
        invalid_in_a_row = 0
        decision = result.output
        if decision is None:
            raise InvestigationError("invalid_model_output", "the model returned no decision")
        messages.append(Message(role="assistant", content=decision.model_dump_json()))
        if decision.hypotheses:
            hypotheses = decision.hypotheses
        summary = decision.summary or summary

        match decision.kind:
            case "request_evidence":
                if decision.template not in allowed:
                    invalid += 1
                    tell(
                        f"Template {fence(repr(decision.template))} is not allowed. "
                        f"Allowed templates: {', '.join(sorted(allowed))}."
                    )
                    continue
                try:
                    resp = await core.collect_evidence(
                        evidence_pb2.CollectEvidenceRequest(
                            lease_id=job.lease_id,
                            template=decision.template,
                            params=decision.params,
                        )
                    )
                except grpc.RpcError as e:
                    if e.code() == grpc.StatusCode.RESOURCE_EXHAUSTED:
                        tell("The evidence budget is spent. Decide with the evidence you have.")
                        continue
                    if e.code() == grpc.StatusCode.INVALID_ARGUMENT:
                        tell(f"The request was refused: {fence(e.details() or '')}")
                        continue
                    raise
                for i in resp.items:
                    level = higher(level, classification(i))
                parts = [render_evidence(i) for i in resp.items]
                parts += [f"Collection failed: {fence(f.message)}" for f in resp.failures]
                tell("\n\n".join(parts) or "No evidence was returned.")
            case "propose":
                if decision.proposal is None:
                    invalid += 1
                    tell("A propose decision needs a proposal.")
                    continue
                resp = await core.submit_proposal(
                    proposal_request(job, decision.proposal, step, provenance)
                )
                if resp.status == proposal_pb2.PROPOSAL_STATUS_ACCEPTED:
                    return done("proposed", step, [resp.proposal_id])
                reason = proposal_pb2.RejectionReason.Name(resp.rejection_reason)
                tell(f"The proposal was rejected ({reason}): {fence(resp.message)}")
            case "conclude_inconclusive":
                return done("inconclusive", step, [])
    return done("inconclusive", max_steps, [])
