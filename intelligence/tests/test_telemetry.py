import asyncio
import json
import logging
from collections.abc import Iterator
from typing import Any

import grpc
import pytest

from tests.investigation.fakes import FakeRpcError
from tests.test_worker import LeasingCore, SlowModel
from themis.v1 import investigation_pb2
from themis_intelligence import telemetry
from themis_intelligence.worker import Worker


class Lines(logging.Handler):
    """Keeps each record as the JSON object the formatter writes."""

    def __init__(self) -> None:
        super().__init__()
        self.setFormatter(telemetry.JSONFormatter())
        self.lines: list[dict[str, Any]] = []

    def emit(self, record: logging.LogRecord) -> None:
        self.lines.append(json.loads(self.format(record)))


@pytest.fixture
def logs() -> Iterator[Lines]:
    handler = Lines()
    root = logging.getLogger()
    root.addHandler(handler)
    level = root.level
    root.setLevel(logging.DEBUG)
    yield handler
    root.removeHandler(handler)
    root.setLevel(level)


def test_log_line_is_json_with_incident_id(logs: Lines) -> None:
    with telemetry.fields(incident="inc-1", lease="lease-1"):
        logging.getLogger("t").warning("lease lost", extra={"reason": "expired"})
    line = logs.lines[-1]
    assert line["msg"] == "lease lost"
    assert line["level"] == "WARN"
    assert line["component"] == "intelligence"
    assert line["incident_id"] == "inc-1" and line["lease_id"] == "lease-1"
    assert line["reason"] == "expired"
    assert "time" in line


def test_fields_end_with_their_block(logs: Lines) -> None:
    with telemetry.fields(incident="inc-1", lease="lease-1"):
        pass
    logging.getLogger("t").info("idle")
    assert "incident_id" not in logs.lines[-1]


def test_exception_is_logged_as_error_and_stack(logs: Lines) -> None:
    try:
        raise ValueError("boom")
    except ValueError:
        logging.getLogger("t").exception("investigation crashed")
    line = logs.lines[-1]
    assert line["level"] == "ERROR"
    assert line["error"] == "ValueError: boom"
    assert "Traceback" in line["stack"]


class TwoLeases(LeasingCore):
    """Leases two incidents, and fails the first renewal of each lease once."""

    def __init__(self) -> None:
        super().__init__(leases=2)
        self.renew_failed: set[str] = set()

    async def lease(self, worker_id: str) -> investigation_pb2.Investigation | None:
        j = await super().lease(worker_id)
        if j is not None:
            j.incident_id, j.lease_id = f"inc-{self.leases}", f"lease-{self.leases}"
        return j

    async def renew_lease(self, lease_id: str) -> None:
        if lease_id not in self.renew_failed:
            self.renew_failed.add(lease_id)
            raise FakeRpcError(grpc.StatusCode.UNAVAILABLE, "scripted")


async def test_context_does_not_leak_between_investigations(logs: Lines) -> None:
    core = TwoLeases()

    def model() -> SlowModel:
        return SlowModel({"kind": "request_evidence", "template": "pod_states", "params": {}})

    worker = Worker(core=core, new_model=model, heartbeat_s=0.05)
    await asyncio.gather(worker.run_once(), worker.run_once())

    by_lease: dict[str, set[str]] = {}
    for line in logs.lines:
        if "lease_id" in line:
            by_lease.setdefault(line["lease_id"], set()).add(line["incident_id"])
    assert by_lease == {"lease-1": {"inc-1"}, "lease-0": {"inc-0"}}
    # Each heartbeat logged its retry with its own investigation's fields.
    retries = [line for line in logs.lines if line["msg"] == "cannot renew the lease, retrying"]
    assert {line["lease_id"] for line in retries} == {"lease-0", "lease-1"}


async def test_context_follows_to_thread(logs: Lines) -> None:
    with telemetry.fields(incident="inc-1", lease="lease-1"):
        await asyncio.to_thread(logging.getLogger("t").info, "from a thread")
    assert logs.lines[-1]["incident_id"] == "inc-1"
