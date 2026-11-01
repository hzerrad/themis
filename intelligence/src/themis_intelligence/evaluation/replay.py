"""A core that answers from a bundle, and a model wrapper that keeps count."""

from dataclasses import dataclass, field

import grpc
from pydantic import BaseModel

from themis.v1 import evidence_pb2, proposal_pb2
from themis_intelligence.evaluation.bundle import Bundle, item_proto
from themis_intelligence.investigation.loop import Model
from themis_intelligence.platform.contract import (
    Classification,
    ModelRequest,
    ModelResult,
    Usage,
)


class BudgetSpentError(grpc.RpcError):
    """What the core raises once the evidence budget is spent."""

    def code(self) -> grpc.StatusCode:
        return grpc.StatusCode.RESOURCE_EXHAUSTED

    def details(self) -> str:
        return "the evidence budget is spent"


class ReplayCore:
    """Serves the bundle's evidence and accepts the first proposal, as the model made it.

    It doesn't repeat the core's checks: a proposal naming the wrong workload is
    scored as harmful, though the real core would refuse it. Params are ignored,
    so asking for another pod's logs returns the logs the core picks by default.
    """

    def __init__(self, bundle: Bundle) -> None:
        self.bundle = bundle
        self.shown = {i.id for i in bundle.initial}
        self.requests = 0
        self.proposals: list[proposal_pb2.SubmitProposalRequest] = []

    async def collect_evidence(
        self, req: evidence_pb2.CollectEvidenceRequest
    ) -> evidence_pb2.CollectEvidenceResponse:
        if self.requests >= self.bundle.max_evidence_requests:
            raise BudgetSpentError()
        self.requests += 1
        items = {i.template: i for i in self.bundle.initial} | self.bundle.requestable
        item = items.get(req.template)
        if item is None:
            return evidence_pb2.CollectEvidenceResponse()
        self.shown.add(item.id)
        return evidence_pb2.CollectEvidenceResponse(items=[item_proto(self.bundle, item)])

    async def submit_proposal(
        self, req: proposal_pb2.SubmitProposalRequest
    ) -> proposal_pb2.SubmitProposalResponse:
        self.proposals.append(req)
        return proposal_pb2.SubmitProposalResponse(
            status=proposal_pb2.PROPOSAL_STATUS_ACCEPTED,
            proposal_id=f"replay-{len(self.proposals)}",
        )


@dataclass
class ModelUse:
    """What the model did across one investigation."""

    calls: int = 0
    # Calls answered with valid output without a repair.
    clean: int = 0
    repaired: int = 0
    usage: Usage = field(default_factory=Usage)
    latency_ms: int = 0
    model: str = ""
    # What the model chose at each step, in order.
    decisions: list[str] = field(default_factory=list[str])


class RecordingModel:
    def __init__(self, model: Model) -> None:
        self.model = model
        self.use = ModelUse()

    @property
    def ceiling(self) -> Classification:
        return self.model.ceiling

    async def complete[T: BaseModel](self, req: ModelRequest, schema: type[T]) -> ModelResult[T]:
        result = await self.model.complete(req, schema)
        use = self.use
        use.calls += 1
        use.clean += result.error is None and result.repair_attempts == 0
        use.repaired += result.repair_attempts > 0
        use.usage = use.usage + result.usage
        use.latency_ms += result.latency_ms
        use.model = result.model
        if result.output is not None:
            use.decisions.append(str(getattr(result.output, "kind", "")))
        return result
