"""A scripted model and an in-memory core for loop tests."""

import json
from typing import Any

import grpc
from pydantic import BaseModel

from themis.v1 import common_pb2, evidence_pb2, investigation_pb2, proposal_pb2
from themis_intelligence.investigation.schemas import StepDecision
from themis_intelligence.platform.contract import ModelRequest, ModelResult, TypedError, Usage

TARGET = common_pb2.TargetRef(
    group="apps", kind="Deployment", namespace="shop", name="checkout", uid="uid-checkout"
)


def item(
    id: str, template: str, data: dict[str, Any], sensitive: bool = False
) -> common_pb2.EvidenceItem:
    return common_pb2.EvidenceItem(
        id=id,
        template=template,
        target=TARGET,
        content_type="application/json",
        data=json.dumps(data).encode(),
        classification=common_pb2.DATA_CLASSIFICATION_SENSITIVE
        if sensitive
        else common_pb2.DATA_CLASSIFICATION_INTERNAL,
    )


def job() -> investigation_pb2.Investigation:
    return investigation_pb2.Investigation(
        lease_id="lease-1",
        incident_id="inc-1",
        profile="rollout-regression",
        target=TARGET,
        initial_evidence=[
            item("ev-history", "rollout_history", {"deployment": {"currentRevision": "2"}}),
            item("ev-pods", "pod_states", {"pods": [{"lastTermination": {"reason": "OOMKilled"}}]}),
        ],
        predicates=[
            common_pb2.PredicateResult(
                name="oom_killed_new_revision",
                value=common_pb2.TRUTH_TRUE,
                reason="x",
                evidence_ids=["ev-pods"],
            )
        ],
        allowed_templates=[
            investigation_pb2.EvidenceTemplate(name="pod_states", description="pods"),
            investigation_pb2.EvidenceTemplate(
                name="container_logs_tail", description="logs", params=["pod"]
            ),
        ],
        candidate_capabilities=["deployment.rollback-to-revision"],
        budget=common_pb2.Budget(max_steps=6),
    )


class ScriptedModel:
    """Replies with decisions in order; an error string becomes a typed error."""

    def __init__(self, *replies: dict[str, Any] | str) -> None:
        self.replies = list(replies)
        self.requests: list[ModelRequest] = []

    async def complete[T: BaseModel](self, req: ModelRequest, schema: type[T]) -> ModelResult[T]:
        self.requests.append(req)
        result = ModelResult[T](
            model="scripted",
            endpoint="fake",
            route="default",
            prompt_version=req.prompt_version,
            schema_version=req.schema_version,
            usage=Usage(input_tokens=100, output_tokens=20),
        )
        reply = self.replies.pop(0) if self.replies else {"kind": "conclude_inconclusive"}
        if isinstance(reply, str):
            result.error = TypedError(kind=reply, message="scripted")  # type: ignore[arg-type]
        else:
            result.output = schema.model_validate(reply)
        assert schema is StepDecision
        return result


class FakeCore:
    def __init__(self, logs: str = "loading catalog\nkilled") -> None:
        self.collected: list[evidence_pb2.CollectEvidenceRequest] = []
        self.submitted: list[proposal_pb2.SubmitProposalRequest] = []
        self.logs = logs
        self.reject_with: proposal_pb2.RejectionReason | None = None
        self.renewals = 0

    async def collect_evidence(
        self, req: evidence_pb2.CollectEvidenceRequest
    ) -> evidence_pb2.CollectEvidenceResponse:
        self.collected.append(req)
        if req.template not in ("pod_states", "container_logs_tail"):
            raise AssertionError(f"the loop forwarded template {req.template}")
        return evidence_pb2.CollectEvidenceResponse(
            items=[
                item(f"ev-{len(self.collected)}", req.template, {"logs": self.logs}, sensitive=True)
            ],
            remaining_requests=3,
        )

    async def submit_proposal(
        self, req: proposal_pb2.SubmitProposalRequest
    ) -> proposal_pb2.SubmitProposalResponse:
        self.submitted.append(req)
        if self.reject_with is not None:
            return proposal_pb2.SubmitProposalResponse(
                proposal_id="p-rejected",
                status=proposal_pb2.PROPOSAL_STATUS_REJECTED,
                rejection_reason=self.reject_with,
                message="rejected by the core",
            )
        return proposal_pb2.SubmitProposalResponse(
            proposal_id="p-1", status=proposal_pb2.PROPOSAL_STATUS_ACCEPTED
        )

    async def renew_lease(self, lease_id: str) -> None:
        self.renewals += 1


class AbortingCore(FakeCore):
    """Fails evidence requests as the core does when the budget is spent."""

    async def collect_evidence(
        self, req: evidence_pb2.CollectEvidenceRequest
    ) -> evidence_pb2.CollectEvidenceResponse:
        raise FakeRpcError(grpc.StatusCode.RESOURCE_EXHAUSTED, "the evidence budget is spent")


class FakeRpcError(grpc.RpcError):
    def __init__(self, code: grpc.StatusCode, details: str) -> None:
        self._code, self._details = code, details

    def code(self) -> grpc.StatusCode:
        return self._code

    def details(self) -> str:
        return self._details
