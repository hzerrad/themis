"""The investigator process: lease, investigate with a heartbeat, report."""

import asyncio
import contextlib
import logging
import socket
from collections.abc import Callable
from typing import Protocol

import grpc

from themis.v1 import investigation_pb2
from themis_intelligence.investigation.loop import Core, InvestigationError, Model, investigate

log = logging.getLogger(__name__)

DEFAULT_MAX_STEPS = 8

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
    ) -> None:
        self.core = core
        self.new_model = new_model
        self.worker_id = worker_id or socket.gethostname()
        self.heartbeat_s = heartbeat_s

    async def run(self, idle_s: float = 2.0) -> None:
        while True:
            try:
                worked = await self.run_once()
            except grpc.RpcError as e:
                log.warning("the core is unavailable: %s", e)
                worked = False
            if not worked:
                await asyncio.sleep(idle_s)

    async def run_once(self) -> bool:
        """Investigates one leased incident; False when there was nothing to lease."""
        job = await self.core.lease(self.worker_id)
        if job is None:
            return False
        log.info("investigating incident %s", job.incident_id)
        heartbeat = asyncio.create_task(self._heartbeat(job.lease_id))
        try:
            result = await investigate(
                job, self.core, self.new_model(), job.budget.max_steps or DEFAULT_MAX_STEPS
            )
        except InvestigationError as e:
            log.warning("investigation of %s failed: %s", job.incident_id, e)
            await self.core.fail(
                investigation_pb2.FailInvestigationRequest(
                    lease_id=job.lease_id, reason=_FAILURES[e.reason], message=str(e)
                )
            )
            return True
        except Exception as e:
            log.exception("investigation of %s crashed", job.incident_id)
            await self.core.fail(
                investigation_pb2.FailInvestigationRequest(
                    lease_id=job.lease_id,
                    reason=investigation_pb2.INVESTIGATION_FAILURE_REASON_INTERNAL,
                    message=f"{type(e).__name__}: {e}",
                )
            )
            return True
        finally:
            heartbeat.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await heartbeat

        await self.core.complete(
            investigation_pb2.CompleteInvestigationRequest(
                lease_id=job.lease_id,
                conclusion=_CONCLUSIONS[result.conclusion],
                summary=result.summary,
                hypotheses=result.hypotheses,
                usage=result.usage,
                provenance=result.provenance,
            )
        )
        log.info("incident %s: %s", job.incident_id, result.conclusion)
        return True

    async def _heartbeat(self, lease_id: str) -> None:
        while True:
            await asyncio.sleep(self.heartbeat_s)
            try:
                await self.core.renew_lease(lease_id)
            except grpc.RpcError as e:
                log.warning("cannot renew lease %s: %s", lease_id, e)
                return
