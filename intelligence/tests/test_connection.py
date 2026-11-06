"""Tests of the model's connection, against fake endpoints for both adapters:
nothing reaches a real model."""

import asyncio
import json
from collections.abc import Awaitable, Callable
from datetime import timedelta
from typing import Any

import anthropic
import grpc
import httpx
import httpx2
import pytest
import respx

from tests.investigation.fakes import FakeRpcError
from themis.v1 import investigation_pb2 as pb
from themis_intelligence.connection import OUTPUT_LIMIT, ConnectionTester, run_test
from themis_intelligence.model_source import Installed
from themis_intelligence.platform.adapters import Provider, ProviderAdapter
from themis_intelligence.platform.adapters.anthropic_messages import AnthropicAdapter
from themis_intelligence.platform.client import Endpoint

BASE = "http://model.test/v1"
KEY = "sk-under-test-0123456789"
NOTHING = Installed(base_url=None, model=None, api_key=None)
# The install's sampling settings, as OpenAI's newer models need them.
TEMPLATE = Endpoint(
    name="default",
    model="",
    ceiling="sensitive",
    output_limit_field="max_completion_tokens",
    temperature=None,
)


def model_test(timeout_s: float = 5.0, **settings: Any) -> pb.ModelTest:
    t = pb.ModelTest(id="t-1", settings=pb.GetModelSettingsResponse(**settings))
    t.timeout.FromTimedelta(timedelta(seconds=timeout_s))
    return t


# A fake endpoint answers each request: (status, body), or a coroutine that
# never answers in time.
Answer = Callable[[], Awaitable[None]] | tuple[int, dict[str, Any]]


def completion(content: str) -> dict[str, Any]:
    return {
        "model": "gpt-test-2026",
        "choices": [{"message": {"role": "assistant", "content": content}}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5},
    }


def claude_message(text: str) -> dict[str, Any]:
    return {
        "id": "msg_1",
        "type": "message",
        "role": "assistant",
        "model": "claude-haiku-5-5",
        "content": [{"type": "text", "text": text}],
        "stop_reason": "end_turn",
        "stop_sequence": None,
        "usage": {"input_tokens": 10, "output_tokens": 5},
    }


async def never() -> None:
    await asyncio.sleep(5)


OPENAI_ERRORS = {
    401: {"error": {"message": f"Incorrect API key provided: {KEY[:6]}****", "type": "x"}},
    404: {"error": {"message": "The model `gpt-4.2` does not exist", "code": "model_not_found"}},
}
ANTHROPIC_ERRORS = {
    401: {
        "type": "error",
        "error": {"type": "authentication_error", "message": "invalid x-api-key"},
    },
    404: {"type": "error", "error": {"type": "not_found_error", "message": "model: gpt-4.2"}},
}


async def openai_test(answer: Answer) -> tuple[pb.ReportModelTestRequest, list[dict[str, Any]]]:
    """Tests an OpenAI-compatible endpoint set on the Setup page, key typed but not saved."""
    sent: list[dict[str, Any]] = []

    async def endpoint(req: httpx.Request) -> httpx.Response:
        sent.append({"body": json.loads(req.content), "auth": req.headers.get("authorization")})
        if callable(answer):
            await answer()
        status, body = answer if not callable(answer) else (200, {})
        return httpx.Response(status, json=body)

    with respx.mock:
        respx.post(f"{BASE}/chat/completions").mock(side_effect=endpoint)
        test = model_test(
            timeout_s=0.2 if callable(answer) else 5.0,
            provider="openai",
            base_url=BASE + "/",
            model="gpt-4.2",
            api_key=KEY,
            schema_dialect="openai-strict",
        )
        return await run_test(test, NOTHING, TEMPLATE), sent


async def anthropic_test(answer: Answer) -> tuple[pb.ReportModelTestRequest, list[dict[str, Any]]]:
    """Tests Anthropic, chosen on the Setup page, with the install's key."""
    sent: list[dict[str, Any]] = []

    async def endpoint(req: httpx2.Request) -> httpx2.Response:
        sent.append({"body": json.loads(req.content), "auth": req.headers.get("authorization")})
        if callable(answer):
            await answer()
        status, body = answer if not callable(answer) else (200, {})
        return httpx2.Response(status, json=body)

    def fake(provider: Provider, base_url: str | None, key: str | None) -> ProviderAdapter:
        assert (provider, base_url) == ("anthropic", None)
        sdk = anthropic.AsyncAnthropic(
            api_key=key,
            base_url="http://claude.test",
            max_retries=0,
            http_client=anthropic.DefaultAsyncHttpxClient(transport=httpx2.MockTransport(endpoint)),
        )
        return AnthropicAdapter(api_key=key, client=sdk)

    installed = Installed(base_url=None, model=None, api_key=KEY)
    test = model_test(
        timeout_s=0.2 if callable(answer) else 5.0,
        provider="anthropic",
        base_url="http://left-from-openai/v1",
        model="gpt-4.2",
    )
    return await run_test(test, installed, TEMPLATE, fake), sent


ADAPTERS = {
    "openai": (openai_test, completion, OPENAI_ERRORS),
    "anthropic": (anthropic_test, claude_message, ANTHROPIC_ERRORS),
}


@pytest.mark.parametrize("adapter", ADAPTERS)
async def test_a_model_that_answers_in_the_reply_format_is_connected(adapter: str) -> None:
    run, reply, _ = ADAPTERS[adapter]
    report, sent = await run((200, reply('{"answer": "ok"}')))
    assert report.outcome == pb.MODEL_TEST_OUTCOME_CONNECTED
    assert report.answered_by in ("gpt-test-2026", "claude-haiku-5-5")
    assert report.model == "gpt-4.2" and report.provider == adapter
    assert report.latency.ToTimedelta() > timedelta(0)
    [request] = sent
    if adapter == "openai":
        # The real request: the endpoint's address, key, dialect, limit field and
        # temperature, with a small output limit and the schema closed.
        assert report.address == BASE and request["auth"] == f"Bearer {KEY}"
        body = request["body"]
        assert body["max_completion_tokens"] == OUTPUT_LIMIT and "temperature" not in body
        schema = body["response_format"]["json_schema"]
        assert schema["strict"] and schema["schema"]["additionalProperties"] is False
        assert schema["schema"]["required"] == ["answer", "note"]
    else:
        # Setup's address, made for another provider, doesn't apply.
        assert report.address == "" and request["auth"] == f"Bearer {KEY}"
        assert request["body"]["max_tokens"] == OUTPUT_LIMIT


@pytest.mark.parametrize("adapter", ADAPTERS)
@pytest.mark.parametrize("status", [401, 404])
async def test_a_refusal_carries_its_status_and_never_the_key(adapter: str, status: int) -> None:
    run, _, errors = ADAPTERS[adapter]
    report, _ = await run((status, errors[status]))
    assert report.outcome == pb.MODEL_TEST_OUTCOME_REFUSED and report.status == status
    assert KEY not in str(report) and KEY[:6] not in report.detail
    if status == 404:
        assert "gpt-4.2" in report.detail


@pytest.mark.parametrize("adapter", ADAPTERS)
async def test_no_answer_in_time(adapter: str) -> None:
    run, _, _ = ADAPTERS[adapter]
    report, _ = await run(never)
    assert report.outcome == pb.MODEL_TEST_OUTCOME_NO_ANSWER
    assert report.latency.ToTimedelta() == timedelta(seconds=0.2)


@pytest.mark.parametrize("adapter", ADAPTERS)
async def test_an_answer_not_in_the_reply_format(adapter: str) -> None:
    run, reply, _ = ADAPTERS[adapter]
    report, _ = await run((200, reply('{"verdict": "fine"}')))
    assert report.outcome == pb.MODEL_TEST_OUTCOME_NOT_IN_FORMAT


@respx.mock
async def test_an_unreachable_address() -> None:
    respx.post(f"{BASE}/chat/completions").mock(
        side_effect=httpx.ConnectError("[Errno 61] Connection refused")
    )
    report = await run_test(model_test(base_url=BASE, model="qwen3:8b"), NOTHING, TEMPLATE)
    assert report.outcome == pb.MODEL_TEST_OUTCOME_UNREACHABLE
    assert report.detail == "Connection refused"


# The install's settings win over the form's, as in an investigation, and
# the install's key is the one used.
@respx.mock
async def test_the_install_wins_over_the_settings_under_test() -> None:
    route = respx.post("http://ollama:11434/v1/chat/completions").mock(
        return_value=httpx.Response(200, json=completion('{"answer": "ok"}'))
    )
    installed = Installed(
        base_url="http://ollama:11434/v1", model=None, api_key="install-key", strict_output=False
    )
    test = model_test(provider="anthropic", base_url=BASE, model="qwen3:8b", api_key=KEY)
    report = await run_test(test, installed, TEMPLATE)
    assert report.outcome == pb.MODEL_TEST_OUTCOME_CONNECTED
    assert (report.provider, report.address, report.strict_output) == (
        "openai",
        "http://ollama:11434/v1",
        False,
    )
    assert route.calls.last.request.headers["authorization"] == "Bearer install-key"


async def test_settings_that_lack_a_model_send_nothing() -> None:
    def no_connection(*_: object) -> ProviderAdapter:
        raise AssertionError("connected")

    report = await run_test(model_test(base_url=BASE), NOTHING, TEMPLATE, no_connection)
    assert report.outcome == pb.MODEL_TEST_OUTCOME_INCOMPLETE


class CoreWithTests:
    """Hands out the tests it holds, then says it asks for none."""

    def __init__(self, *tests: pb.ModelTest) -> None:
        self.tests = list(tests)
        self.reports: list[pb.ReportModelTestRequest] = []

    async def take_model_test(self) -> pb.ModelTest | None:
        if not self.tests:
            raise FakeRpcError(grpc.StatusCode.UNIMPLEMENTED, "no tests")
        return self.tests.pop(0)

    async def report_model_test(self, req: pb.ReportModelTestRequest) -> None:
        self.reports.append(req)


# The tester reports each test it takes, even one that fails in the
# investigator, and stops when the core asks for none.
async def test_the_tester_reports_every_test() -> None:
    def broken(*_: object) -> ProviderAdapter:
        raise RuntimeError("no adapter")

    core = CoreWithTests(model_test(base_url=BASE, model="m"), model_test())
    await asyncio.wait_for(ConnectionTester(core, NOTHING, TEMPLATE, broken).run(), timeout=5)
    assert [r.outcome for r in core.reports] == [
        pb.MODEL_TEST_OUTCOME_ERROR,
        pb.MODEL_TEST_OUTCOME_INCOMPLETE,
    ]
