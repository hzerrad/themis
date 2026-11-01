"""The investigator process: lease, investigate with a heartbeat, report."""

import asyncio
import contextlib
import logging
import socket
from collections.abc import Awaitable, Callable
from typing import Protocol, cast

import grpc

from themis.v1 import investigation_pb2
from themis_intelligence import telemetry, tracing
from themis_intelligence.investigation.loop import Core, InvestigationError, Model, investigate

log = logging.getLogger(__name__)

DEFAULT_MAX_STEPS = 8
REPORT_ATTEMPTS = 3

_TRANSIENT = {grpc.StatusCode.UNAVAILABLE, grpc.StatusCode.DEADLINE_EXCEEDED}

_CONCLUSIONS = {
    "proposed": investigation_pb2.INVESTIGATION_CONCLUSION_PROPOSED,
    "inconclusive": investigation_pb2.INVESTIGATION_CONCLUSION_INCONCLUSIVE,
}

_FAILURES = {
    "model_unavailable": investigation_pb2.INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE,
    "invalid_model_output": investigation_pb2.INVESTIGATION_FAILURE_REASON_INVALID_MODEL_OUTPUT,
    "budget_exhausted": investigation_pb2.INVESTIGATION_FAILURE_REASON_BUDGET_EXHAUSTED,
    "deadline_exceeded": investigation_pb2.INVESTIGATION_FAILURE_REASON_DEADLINE_EXCEEDED,
}


def rpc_error(e: grpc.RpcError) -> str:
    """A gRPC error as its code and the core's message."""
    call = cast(grpc.Call, e)
    return f"{call.code().name}: {call.details()}"


class WorkerCore(Core, Protocol):
    async def lease(self, worker_id: str) -> investigation_pb2.Investigation | None: ...

    async def renew_lease(self, lease_id: str) -> None: ...

    async def complete(self, req: investigation_pb2.CompleteInvestigationRequest) -> None: ...

    async def fail(self, req: investigation_pb2.FailInvestigationRequest) -> None: ...


class Worker:
    def __init__(
        self,
        core: WorkerCore,
        new_model: Callable[[], Model],
        worker_id: str | None = None,
        heartbeat_s: float = 20.0,
        retry_s: float = 2.0,
    ) -> None:
        self.core = core
        self.new_model = new_model
        self.worker_id = worker_id or socket.gethostname()
        self.heartbeat_s = heartbeat_s
        self.retry_s = retry_s

    async def run(self, idle_s: float = 2.0) -> None:
        while True:
            try:
                worked = await self.run_once()
            except grpc.RpcError as e:
                log.warning("core unavailable", extra={"error": rpc_error(e)})
                worked = False
            if not worked:
                await asyncio.sleep(idle_s)

    async def run_once(self) -> bool:
        """Investigates one leased incident; False when there was nothing to lease."""
        job = await self.core.lease(self.worker_id)
        if job is None:
            return False
        # Set before the investigation's tasks are created, which copy them.
        with (
            telemetry.fields(incident=job.incident_id, lease=job.lease_id),
            tracing.tracer.start_as_current_span(
                "investigation",
                attributes={"themis.incident.id": job.incident_id, "themis.lease.id": job.lease_id},
            ),
        ):
            await self._investigate(job)
        return True

    async def _investigate(self, job: investigation_pb2.Investigation) -> None:
        log.info("investigation started")
        deadline = job.budget.deadline.ToSeconds() if job.budget.HasField("deadline") else None
        gaps: list[investigation_pb2.EvidenceGap] = []
        task = asyncio.create_task(
            investigate(
                job,
                self.core,
                self.new_model(),
                job.budget.max_steps or DEFAULT_MAX_STEPS,
                deadline,
                gaps,
            )
        )
        lost = asyncio.Event()
        heartbeat = asyncio.create_task(self._heartbeat(job.lease_id, task, lost))
        try:
            result = await task
        except asyncio.CancelledError:
            if not lost.is_set():
                raise
            log.warning("lease lost, investigation abandoned")
            return
        except InvestigationError as e:
            # The reason only: a model error's message can quote model output.
            log.warning("investigation failed", extra={"reason": e.reason})
            await self._report(
                self.core.fail,
                investigation_pb2.FailInvestigationRequest(
                    lease_id=job.lease_id, reason=_FAILURES[e.reason], message=str(e), gaps=gaps
                ),
            )
            return
        except Exception as e:
            log.exception("investigation crashed")
            await self._report(
                self.core.fail,
                investigation_pb2.FailInvestigationRequest(
                    lease_id=job.lease_id,
                    reason=investigation_pb2.INVESTIGATION_FAILURE_REASON_INTERNAL,
                    message=f"{type(e).__name__}: {e}",
                    gaps=gaps,
                ),
            )
            return
        finally:
            heartbeat.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await heartbeat

        await self._report(
            self.core.complete,
            investigation_pb2.CompleteInvestigationRequest(
                lease_id=job.lease_id,
                conclusion=_CONCLUSIONS[result.conclusion],
                summary=result.summary,
                hypotheses=result.hypotheses,
                usage=result.usage,
                provenance=result.provenance,
                gaps=gaps,
            ),
        )
        log.info("investigation completed", extra={"conclusion": result.conclusion})

    async def _report[R](self, call: Callable[[R], Awaitable[None]], req: R) -> None:
        """Retries transient errors, so a finished result is not lost to one."""
        for attempt in range(REPORT_ATTEMPTS):
            try:
                await call(req)
                return
            except grpc.RpcError as e:
                if e.code() not in _TRANSIENT or attempt == REPORT_ATTEMPTS - 1:
                    log.warning("cannot report to the core", extra={"error": rpc_error(e)})
                    return
                await asyncio.sleep(self.retry_s * (attempt + 1))

    async def _heartbeat(
        self, lease_id: str, task: asyncio.Task[object], lost: asyncio.Event
    ) -> None:
        """Renews the lease; when the core says it is lost, stops the investigation."""
        while True:
            await asyncio.sleep(self.heartbeat_s)
            try:
                await self.core.renew_lease(lease_id)
            except grpc.RpcError as e:
                if e.code() in _TRANSIENT:
                    log.warning("cannot renew the lease, retrying", extra={"error": rpc_error(e)})
                    continue
                lost.set()
                task.cancel()
                return
