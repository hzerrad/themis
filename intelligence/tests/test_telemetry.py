import asyncio
import json
import logging
import socket
import urllib.request
from collections.abc import Iterator
from typing import Any

import grpc
import httpx
import pytest
import respx
from prometheus_client import REGISTRY
from pydantic import BaseModel

from tests.investigation.fakes import FakeRpcError, ScriptedModel
from tests.test_worker import LeasingCore, SlowModel
from themis.v1 import investigation_pb2
from themis_intelligence import telemetry
from themis_intelligence.__main__ import parse_args, read_price
from themis_intelligence.platform.adapters.openai_compat import OpenAICompatAdapter
from themis_intelligence.platform.budget import Budget
from themis_intelligence.platform.client import Endpoint, ModelClient, Price
from themis_intelligence.platform.contract import Message, ModelRequest
from themis_intelligence.rpc.core_client import _CountCalls  # pyright: ignore[reportPrivateUsage]
from themis_intelligence.worker import Worker

BASE = "http://model.test/v1"


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


def value(name: str, **labels: str) -> float:
    return REGISTRY.get_sample_value(name, labels) or 0.0


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


class Answer(BaseModel):
    verdict: str


def reply(content: str, prompt: int = 10, completion: int = 5) -> httpx.Response:
    return httpx.Response(
        200,
        json={
            "model": "qwen3",
            "choices": [{"message": {"role": "assistant", "content": content}}],
            "usage": {"prompt_tokens": prompt, "completion_tokens": completion},
        },
    )


def request() -> ModelRequest:
    return ModelRequest(
        task_class="investigation_step",
        incident_id="inc-1",
        trace_id="trace-1",
        schema_version="1",
        data_classification="internal",
        prompt_version="rollout_v1",
        messages=[Message(role="user", content="is it the rollout?")],
        deadline_s=5.0,
        max_output_tokens=100,
    )


def client(route: str, price: Price | None = None) -> ModelClient:
    telemetry.configure_routes(["priced", "free"])
    return ModelClient(
        adapter=OpenAICompatAdapter(base_url=BASE),
        endpoint=Endpoint(name="ollama", model="qwen3", ceiling="sensitive"),
        budget=Budget(max_tokens=10_000),
        route=route,
        price=price,
    )


@respx.mock
async def test_model_call_counts_tokens_and_duration() -> None:
    respx.post(f"{BASE}/chat/completions").mock(return_value=reply('{"verdict": "rollout"}'))
    ok = value("themis_model_requests_total", route="free", outcome="ok")
    tokens = value("themis_model_tokens_total", route="free", kind="input")
    durations = value("themis_model_request_duration_seconds_count", route="free")

    await client("free").complete(request(), Answer)

    assert value("themis_model_requests_total", route="free", outcome="ok") == ok + 1
    assert value("themis_model_tokens_total", route="free", kind="input") == tokens + 10
    assert value("themis_model_request_duration_seconds_count", route="free") == durations + 1


@respx.mock
async def test_cost_counted_only_for_priced_routes() -> None:
    respx.post(f"{BASE}/chat/completions").mock(
        return_value=reply('{"verdict": "rollout"}', prompt=1_000_000, completion=500_000)
    )
    priced = value("themis_model_cost_usd_total", route="priced")

    await client("priced", Price(input_usd_per_mtok=3, output_usd_per_mtok=15)).complete(
        request(), Answer
    )
    await client("free").complete(request(), Answer)

    assert value("themis_model_cost_usd_total", route="priced") == pytest.approx(priced + 10.5)
    assert REGISTRY.get_sample_value("themis_model_cost_usd_total", {"route": "free"}) is None


@respx.mock
async def test_validation_failure_counted() -> None:
    respx.post(f"{BASE}/chat/completions").mock(
        side_effect=[reply('{"wrong": 1}'), reply('{"verdict": "rollout"}')]
    )
    failures = value("themis_model_validation_failures_total", route="free")
    result = await client("free").complete(request(), Answer)
    assert result.error is None
    assert value("themis_model_validation_failures_total", route="free") == failures + 1


@respx.mock
async def test_unknown_route_is_recorded_as_other() -> None:
    respx.post(f"{BASE}/chat/completions").mock(return_value=reply('{"verdict": "rollout"}'))
    other = value("themis_model_requests_total", route="other", outcome="ok")
    await client("not-configured").complete(request(), Answer)
    assert value("themis_model_requests_total", route="other", outcome="ok") == other + 1


async def test_budget_exhaustion_counted_by_kind() -> None:
    steps = value("themis_budget_exhausted_total", kind="steps")
    tokens = value("themis_budget_exhausted_total", kind="tokens")
    evidence: dict[str, Any] = {"kind": "request_evidence", "template": "pod_states", "params": {}}
    worker = Worker(core=LeasingCore(), new_model=lambda: ScriptedModel(*[evidence] * 6))
    await worker.run_once()
    await Worker(core=LeasingCore(), new_model=lambda: ScriptedModel("budget_exceeded")).run_once()
    assert value("themis_budget_exhausted_total", kind="steps") == steps + 1
    assert value("themis_budget_exhausted_total", kind="tokens") == tokens + 1


async def test_investigation_steps_observed() -> None:
    count = value("themis_investigation_steps_count")
    await Worker(core=LeasingCore(), new_model=ScriptedModel).run_once()
    assert value("themis_investigation_steps_count") == count + 1


class Call:
    def __init__(self, code: grpc.StatusCode) -> None:
        self._code = code

    async def code(self) -> grpc.StatusCode:
        return self._code


async def test_core_requests_counted_by_method_and_code() -> None:
    labels = {"method": "RenewLease", "code": "FAILED_PRECONDITION"}
    before = value("themis_core_requests_total", **labels)

    async def continuation(details: Any, request: Any) -> Call:
        return Call(grpc.StatusCode.FAILED_PRECONDITION)

    details = grpc.aio.ClientCallDetails(
        "/themis.v1.InvestigationService/RenewLease", None, None, None, None
    )
    await _CountCalls().intercept_unary_unary(continuation, details, None)
    assert value("themis_core_requests_total", **labels) == before + 1


def test_metric_labels_allowlisted() -> None:
    telemetry.configure_routes(["priced", "free"])
    telemetry.init_route("free", priced=False)
    seen = 0
    for family in REGISTRY.collect():
        if not family.name.startswith("themis_"):
            continue
        seen += 1
        values: dict[str, set[str]] = {}
        for sample in family.samples:
            for name, v in sample.labels.items():
                if name == "le":
                    continue
                assert name in telemetry.METRIC_LABELS, f"{family.name} has label {name}"
                values.setdefault(name, set()).add(v)
        for name, vs in values.items():
            assert len(vs) <= telemetry.LABEL_CEILING, f"{family.name}{{{name}}}: {len(vs)} values"
        routes = values.get("route", set())
        assert routes <= {"priced", "free", telemetry.OTHER}, f"{family.name}: routes {routes}"
    assert seen >= 8


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_metrics_served_on_their_port() -> None:
    port = free_port()
    telemetry.serve_metrics(port)
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/metrics", timeout=5) as resp:
        assert b"themis_budget_exhausted_total" in resp.read()


def test_metrics_server_disabled_with_port_zero(monkeypatch: pytest.MonkeyPatch) -> None:
    started: list[int] = []
    monkeypatch.setattr(telemetry, "start_http_server", started.append)
    telemetry.serve_metrics(0)
    assert started == []


def test_price_needs_both_or_neither(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("MODEL_PRICE_INPUT_USD_PER_MTOK", raising=False)
    monkeypatch.delenv("MODEL_PRICE_OUTPUT_USD_PER_MTOK", raising=False)
    assert read_price() is None
    monkeypatch.setenv("MODEL_PRICE_INPUT_USD_PER_MTOK", "3")
    with pytest.raises(SystemExit):
        read_price()
    monkeypatch.setenv("MODEL_PRICE_OUTPUT_USD_PER_MTOK", "15")
    assert read_price() == Price(input_usd_per_mtok=3, output_usd_per_mtok=15)
    monkeypatch.setenv("MODEL_PRICE_OUTPUT_USD_PER_MTOK", "cheap")
    with pytest.raises(SystemExit):
        read_price()


def test_flags_default_to_metrics_on_8082_at_info() -> None:
    args = parse_args([])
    assert (args.metrics_port, args.log_level) == (8082, "info")
    assert parse_args(["--metrics-port", "0"]).metrics_port == 0
