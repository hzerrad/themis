import json
import logging
from typing import Any

import grpc
import httpx
import pytest
import respx
from opentelemetry import trace
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from pydantic import BaseModel

from tests.investigation.fakes import ScriptedModel
from tests.test_worker import LeasingCore
from themis_intelligence import telemetry, tracing
from themis_intelligence.platform.adapters.openai_compat import OpenAICompatAdapter
from themis_intelligence.platform.budget import Budget
from themis_intelligence.platform.client import Endpoint, ModelClient
from themis_intelligence.platform.contract import Message, ModelRequest
from themis_intelligence.rpc.core_client import _TraceCalls  # pyright: ignore[reportPrivateUsage]
from themis_intelligence.worker import Worker

BASE = "http://model.test/v1"


class Answer(BaseModel):
    verdict: str


@respx.mock
async def test_model_span_has_route_and_tokens_but_no_prompt(spans: InMemorySpanExporter) -> None:
    respx.post(f"{BASE}/chat/completions").mock(
        return_value=httpx.Response(
            200,
            json={
                "model": "qwen3",
                "choices": [{"message": {"content": '{"verdict": "canary-reply"}'}}],
                "usage": {"prompt_tokens": 12, "completion_tokens": 3},
            },
        )
    )
    telemetry.configure_routes(["default"])
    model = ModelClient(
        adapter=OpenAICompatAdapter(base_url=BASE),
        endpoint=Endpoint(name="ollama", model="qwen3", ceiling="sensitive"),
        budget=Budget(max_tokens=10_000),
    )
    await model.complete(
        ModelRequest(
            task_class="investigation_step",
            incident_id="inc-1",
            trace_id="trace-1",
            schema_version="1",
            data_classification="internal",
            prompt_version="rollout_v1",
            messages=[Message(role="user", content="canary-prompt")],
            deadline_s=5.0,
            max_output_tokens=100,
        ),
        Answer,
    )

    (span,) = [s for s in spans.get_finished_spans() if s.name == "model.request"]
    attrs = dict(span.attributes or {})
    assert attrs["themis.model.route"] == "default"
    assert attrs["themis.model.outcome"] == "ok"
    assert attrs["gen_ai.usage.input_tokens"] == 12
    assert attrs["gen_ai.usage.output_tokens"] == 3
    recorded = json.dumps({"attrs": attrs, "events": [e.name for e in span.events]}, default=str)
    assert "canary-prompt" not in recorded and "canary-reply" not in recorded


async def test_investigation_is_one_trace_with_its_ids(spans: InMemorySpanExporter) -> None:
    await Worker(core=LeasingCore(), new_model=ScriptedModel).run_once()

    (root,) = [s for s in spans.get_finished_spans() if s.name == "investigation"]
    attrs = dict(root.attributes or {})
    assert attrs["themis.incident.id"] == "inc-1"
    assert attrs["themis.lease.id"] == "lease-1"
    assert root.parent is None


class Call:
    async def code(self) -> grpc.StatusCode:
        return grpc.StatusCode.OK


async def call_core() -> list[tuple[str, str]]:
    """Calls through the interceptor and returns the metadata it sent."""
    sent: list[tuple[str, str]] = []

    async def continuation(details: grpc.aio.ClientCallDetails, request: Any) -> Call:
        sent.extend(details.metadata or ())  # pyright: ignore[reportArgumentType]
        return Call()

    details = grpc.aio.ClientCallDetails(
        "/themis.v1.InvestigationService/RenewLease",
        None,
        (("authorization", "Bearer t"),),  # pyright: ignore[reportArgumentType]
        None,
        None,
    )
    await _TraceCalls().intercept_unary_unary(continuation, details, None)
    return sent


async def test_core_call_sends_its_trace_inside_a_span(spans: InMemorySpanExporter) -> None:
    with tracing.tracer.start_as_current_span("investigation") as parent:
        sent = dict(await call_core())
    trace_id = f"{parent.get_span_context().trace_id:032x}"
    assert sent["authorization"] == "Bearer t"
    assert trace_id in sent["traceparent"]
    (client,) = [s for s in spans.get_finished_spans() if s.kind == trace.SpanKind.CLIENT]
    assert client.name == "themis.v1.InvestigationService/RenewLease"


# Idle lease polls run outside any span: they make no trace.
async def test_core_call_outside_a_span_is_not_traced(spans: InMemorySpanExporter) -> None:
    sent = dict(await call_core())
    assert "traceparent" not in sent
    assert spans.get_finished_spans() == ()


def test_log_line_carries_trace_id() -> None:
    lines: list[dict[str, Any]] = []

    class Keep(logging.Handler):
        def emit(self, record: logging.LogRecord) -> None:
            lines.append(json.loads(telemetry.JSONFormatter().format(record)))

    log = logging.getLogger("tracing-test")
    log.addHandler(Keep())
    log.setLevel(logging.INFO)
    with tracing.tracer.start_as_current_span("investigation") as span:
        log.info("traced")
    log.info("untraced")
    assert lines[0]["trace_id"] == f"{span.get_span_context().trace_id:032x}"
    assert "trace_id" not in lines[1]


def test_tracing_off_without_an_endpoint(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_ENDPOINT", raising=False)
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", raising=False)
    provider = trace.get_tracer_provider()
    tracing.setup_tracing()()
    assert trace.get_tracer_provider() is provider
