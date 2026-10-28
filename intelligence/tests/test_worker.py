import asyncio
from typing import Any

import grpc

from tests.investigation.fakes import FakeCore, FakeRpcError, ScriptedModel, job
from themis.v1 import investigation_pb2
from themis_intelligence.worker import Worker


class LeasingCore(FakeCore):
    def __init__(self, leases: int = 1) -> None:
        super().__init__()
        self.leases = leases
        self.renew_errors: list[grpc.StatusCode] = []
        self.complete_errors: list[grpc.StatusCode] = []
        self.completed: list[investigation_pb2.CompleteInvestigationRequest] = []
        self.failed: list[investigation_pb2.FailInvestigationRequest] = []

    async def lease(self, worker_id: str) -> investigation_pb2.Investigation | None:
        if self.leases == 0:
            return None
        self.leases -= 1
        return job()

    async def renew_lease(self, lease_id: str) -> None:
        self.renewals += 1
        if self.renew_errors:
            raise FakeRpcError(self.renew_errors.pop(0), "scripted")

    async def complete(self, req: investigation_pb2.CompleteInvestigationRequest) -> None:
        if self.complete_errors:
            raise FakeRpcError(self.complete_errors.pop(0), "scripted")
        self.completed.append(req)

    async def fail(self, req: investigation_pb2.FailInvestigationRequest) -> None:
        self.failed.append(req)


class SlowModel(ScriptedModel):
    async def complete(self, req: Any, schema: Any) -> Any:  # type: ignore[override]
        await asyncio.sleep(0.2)
        return await super().complete(req, schema)


async def test_completes_a_leased_investigation() -> None:
    core = LeasingCore()
    worker = Worker(
        core=core,
        new_model=lambda: ScriptedModel({"kind": "conclude_inconclusive", "summary": "no logs"}),
    )
    assert await worker.run_once() is True
    assert len(core.completed) == 1
    done = core.completed[0]
    assert done.lease_id == "lease-1"
    assert done.conclusion == investigation_pb2.INVESTIGATION_CONCLUSION_INCONCLUSIVE
    assert done.summary == "no logs"
    assert done.provenance.prompt_version == "rollout_v1"


async def test_nothing_to_lease() -> None:
    worker = Worker(core=LeasingCore(leases=0), new_model=ScriptedModel)
    assert await worker.run_once() is False


async def test_failure_is_reported() -> None:
    core = LeasingCore()
    worker = Worker(core=core, new_model=lambda: ScriptedModel("unavailable"))
    await worker.run_once()
    assert core.completed == []
    assert len(core.failed) == 1
    assert core.failed[0].reason == investigation_pb2.INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE


async def test_heartbeat_sent_during_long_investigation() -> None:
    core = LeasingCore()
    model = SlowModel({"kind": "request_evidence", "template": "pod_states", "params": {}})
    worker = Worker(core=core, new_model=lambda: model, heartbeat_s=0.05)
    await worker.run_once()
    assert core.renewals >= 3
    assert len(core.completed) == 1


def slow_investigation() -> SlowModel:
    step: dict[str, Any] = {"kind": "request_evidence", "template": "pod_states", "params": {}}
    return SlowModel(*[step] * 10)


# The core ended the lease, for example because the incident closed: the
# investigation stops instead of spending tokens on it.
async def test_lost_lease_cancels_the_investigation() -> None:
    core = LeasingCore()
    core.renew_errors = [grpc.StatusCode.FAILED_PRECONDITION]
    model = slow_investigation()
    worker = Worker(core=core, new_model=lambda: model, heartbeat_s=0.05)
    assert await worker.run_once() is True
    assert core.completed == [] and core.failed == []
    assert len(model.requests) <= 2


async def test_heartbeat_survives_a_transient_error() -> None:
    core = LeasingCore()
    core.renew_errors = [grpc.StatusCode.UNAVAILABLE]
    model = SlowModel({"kind": "request_evidence", "template": "pod_states", "params": {}})
    worker = Worker(core=core, new_model=lambda: model, heartbeat_s=0.05)
    await worker.run_once()
    assert core.renewals >= 3
    assert len(core.completed) == 1


async def test_report_retried_on_a_transient_error() -> None:
    core = LeasingCore()
    core.complete_errors = [grpc.StatusCode.UNAVAILABLE]
    worker = Worker(
        core=core, new_model=lambda: ScriptedModel({"kind": "conclude_inconclusive"}), retry_s=0.01
    )
    await worker.run_once()
    assert len(core.completed) == 1
