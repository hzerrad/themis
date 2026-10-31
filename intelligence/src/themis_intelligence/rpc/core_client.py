"""themis-core's gRPC API, with the investigator's bearer token on every call."""

from collections.abc import Awaitable, Callable
from typing import Any, Protocol, cast

import grpc
from opentelemetry import propagate, trace
from opentelemetry.trace import SpanKind, Status, StatusCode

from themis.v1 import (
    evidence_pb2,
    evidence_pb2_grpc,
    investigation_pb2,
    investigation_pb2_grpc,
    proposal_pb2,
    proposal_pb2_grpc,
)
from themis_intelligence import telemetry, tracing

CALL_TIMEOUT_S = 30.0

Metadata = tuple[tuple[str, str], ...]


# Typed views of the generated stubs, which carry no annotations.
class _Investigations(Protocol):
    def LeaseInvestigation(
        self,
        request: investigation_pb2.LeaseInvestigationRequest,
        *,
        metadata: Metadata,
        timeout: float,
    ) -> Awaitable[investigation_pb2.LeaseInvestigationResponse]: ...

    def RenewLease(
        self, request: investigation_pb2.RenewLeaseRequest, *, metadata: Metadata, timeout: float
    ) -> Awaitable[investigation_pb2.RenewLeaseResponse]: ...

    def CompleteInvestigation(
        self,
        request: investigation_pb2.CompleteInvestigationRequest,
        *,
        metadata: Metadata,
        timeout: float,
    ) -> Awaitable[investigation_pb2.CompleteInvestigationResponse]: ...

    def FailInvestigation(
        self,
        request: investigation_pb2.FailInvestigationRequest,
        *,
        metadata: Metadata,
        timeout: float,
    ) -> Awaitable[investigation_pb2.FailInvestigationResponse]: ...


class _Evidence(Protocol):
    def CollectEvidence(
        self, request: evidence_pb2.CollectEvidenceRequest, *, metadata: Metadata, timeout: float
    ) -> Awaitable[evidence_pb2.CollectEvidenceResponse]: ...


class _Proposals(Protocol):
    def SubmitProposal(
        self, request: proposal_pb2.SubmitProposalRequest, *, metadata: Metadata, timeout: float
    ) -> Awaitable[proposal_pb2.SubmitProposalResponse]: ...


class _CountCalls(grpc.aio.UnaryUnaryClientInterceptor):  # pyright: ignore[reportMissingTypeArgument]
    """Counts each call by method and status code."""

    async def intercept_unary_unary(
        self,
        continuation: Callable[[grpc.aio.ClientCallDetails, Any], Any],
        client_call_details: grpc.aio.ClientCallDetails,
        request: Any,
    ) -> Any:
        method = client_call_details.method
        if isinstance(method, bytes):
            method = method.decode()
        call = await continuation(client_call_details, request)
        code = await call.code()
        telemetry.core_requests.labels(method.rsplit("/", 1)[-1], code.name).inc()
        return call


class _TraceCalls(grpc.aio.UnaryUnaryClientInterceptor):  # pyright: ignore[reportMissingTypeArgument]
    """Traces calls made inside a span, and sends its trace to the core. Idle lease
    polls run outside any span, so they make no trace."""

    async def intercept_unary_unary(
        self,
        continuation: Callable[[grpc.aio.ClientCallDetails, Any], Any],
        client_call_details: grpc.aio.ClientCallDetails,
        request: Any,
    ) -> Any:
        if not trace.get_current_span().get_span_context().is_valid:
            return await continuation(client_call_details, request)
        method = client_call_details.method
        if isinstance(method, bytes):
            method = method.decode()
        service, _, name = method.lstrip("/").rpartition("/")
        with tracing.tracer.start_as_current_span(
            method.lstrip("/"),
            kind=SpanKind.CLIENT,
            attributes={"rpc.system": "grpc", "rpc.service": service, "rpc.method": name},
        ) as span:
            carrier: dict[str, str] = {}
            propagate.inject(carrier)
            details = grpc.aio.ClientCallDetails(
                client_call_details.method,
                client_call_details.timeout,
                (*(client_call_details.metadata or ()), *carrier.items()),  # pyright: ignore[reportArgumentType]
                client_call_details.credentials,
                client_call_details.wait_for_ready,
            )
            call = await continuation(details, request)
            code = await call.code()
            span.set_attribute("rpc.grpc.status_code", code.value[0])
            if code != grpc.StatusCode.OK:
                span.set_status(Status(StatusCode.ERROR, code.name))
            return call


class CoreClient:
    def __init__(self, target: str, token: str) -> None:
        self.channel = grpc.aio.insecure_channel(
            target, interceptors=[_TraceCalls(), _CountCalls()]
        )
        self.metadata: Metadata = (("authorization", f"Bearer {token}"),)
        self.investigations = cast(
            _Investigations,
            investigation_pb2_grpc.InvestigationServiceStub(self.channel),  # pyright: ignore[reportUnknownMemberType]
        )
        self.evidence = cast(
            _Evidence,
            evidence_pb2_grpc.EvidenceServiceStub(self.channel),  # pyright: ignore[reportUnknownMemberType]
        )
        self.proposals = cast(
            _Proposals,
            proposal_pb2_grpc.ProposalServiceStub(self.channel),  # pyright: ignore[reportUnknownMemberType]
        )

    async def lease(self, worker_id: str) -> investigation_pb2.Investigation | None:
        resp = await self.investigations.LeaseInvestigation(
            investigation_pb2.LeaseInvestigationRequest(worker_id=worker_id),
            metadata=self.metadata,
            timeout=CALL_TIMEOUT_S,
        )
        return resp.investigation if resp.HasField("investigation") else None

    async def renew_lease(self, lease_id: str) -> None:
        await self.investigations.RenewLease(
            investigation_pb2.RenewLeaseRequest(lease_id=lease_id),
            metadata=self.metadata,
            timeout=CALL_TIMEOUT_S,
        )

    async def complete(self, req: investigation_pb2.CompleteInvestigationRequest) -> None:
        await self.investigations.CompleteInvestigation(
            req, metadata=self.metadata, timeout=CALL_TIMEOUT_S
        )

    async def fail(self, req: investigation_pb2.FailInvestigationRequest) -> None:
        await self.investigations.FailInvestigation(
            req, metadata=self.metadata, timeout=CALL_TIMEOUT_S
        )

    async def collect_evidence(
        self, req: evidence_pb2.CollectEvidenceRequest
    ) -> evidence_pb2.CollectEvidenceResponse:
        return await self.evidence.CollectEvidence(
            req, metadata=self.metadata, timeout=CALL_TIMEOUT_S
        )

    async def submit_proposal(
        self, req: proposal_pb2.SubmitProposalRequest
    ) -> proposal_pb2.SubmitProposalResponse:
        return await self.proposals.SubmitProposal(
            req, metadata=self.metadata, timeout=CALL_TIMEOUT_S
        )

    async def close(self) -> None:
        await self.channel.close()
