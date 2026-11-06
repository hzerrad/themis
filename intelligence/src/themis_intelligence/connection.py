"""Tests of the model's connection, asked for on Themis's Setup page.

Only the investigator may reach the model, so the core hands it each test: one
small request with the settings under test, through the adapter and endpoint
settings an investigation uses. A key under test serves that request only: it
is never kept or logged.
"""

import asyncio
import logging
import re
import time
from collections.abc import Callable
from datetime import timedelta
from typing import Literal, Protocol, cast

import anthropic
import grpc
import httpx
from pydantic import BaseModel, ValidationError

from themis.v1 import investigation_pb2
from themis_intelligence.model_source import Installed, resolve
from themis_intelligence.platform.adapters import Provider, ProviderAdapter, connect
from themis_intelligence.platform.client import Endpoint
from themis_intelligence.platform.contract import Message, ProviderError

log = logging.getLogger(__name__)

pb = investigation_pb2


class Ping(BaseModel):
    """The reply asked for. Its optional field makes a provider's strict mode
    treat this schema as it treats an investigation's, whose optional fields
    OpenAI refuses unless they are sent in its own form."""

    answer: Literal["ok"]
    note: str | None = None


MESSAGES = [
    Message(role="user", content='This tests the connection. Reply with {"answer": "ok"}.'),
]

# Room for a short reply. An endpoint's own output limit, set for a model that
# reasons, replaces it, as in an investigation.
OUTPUT_LIMIT = 512
DEFAULT_TIMEOUT_S = 20.0
# The most of a provider's text a report carries.
DETAIL_LIMIT = 300

Connect = Callable[[Provider, str | None, str | None], ProviderAdapter]


async def run_test(
    test: pb.ModelTest,
    installed: Installed,
    template: Endpoint,
    connect: Connect = connect,
) -> pb.ReportModelTestRequest:
    """Sends the model one request with the settings under test, and says what
    happened."""
    report = pb.ReportModelTestRequest(id=test.id)
    got = resolve(installed, test.settings, template)
    if got is None:
        report.outcome = pb.MODEL_TEST_OUTCOME_INCOMPLETE
        return report
    endpoint = got.endpoint
    report.provider, report.address, report.model = got.provider, got.base_url or "", endpoint.model
    report.strict_output, report.schema_dialect = endpoint.strict_output, endpoint.schema_dialect
    timeout_s = test.timeout.ToTimedelta().total_seconds() or DEFAULT_TIMEOUT_S
    limit = endpoint.max_output_tokens or OUTPUT_LIMIT
    detail = ""
    adapter = connect(got.provider, got.base_url, got.api_key)
    started = time.monotonic()
    try:
        async with asyncio.timeout(timeout_s):
            resp = await adapter.send(endpoint.request(MESSAGES, Ping.model_json_schema(), limit))
    except TimeoutError:
        report.outcome = pb.MODEL_TEST_OUTCOME_NO_ANSWER
    except Exception as e:
        report.outcome, report.status, detail = _failure(e)
    else:
        if resp.truncated:
            report.outcome, report.output_limit = pb.MODEL_TEST_OUTCOME_CUT_OFF, limit
        elif resp.refused:
            report.outcome = pb.MODEL_TEST_OUTCOME_DECLINED
        else:
            try:
                Ping.model_validate_json(resp.content)
                report.outcome, report.answered_by = pb.MODEL_TEST_OUTCOME_CONNECTED, resp.model
            except ValidationError:
                report.outcome = pb.MODEL_TEST_OUTCOME_NOT_IN_FORMAT
    finally:
        elapsed = time.monotonic() - started
        await adapter.aclose()
    if report.outcome == pb.MODEL_TEST_OUTCOME_NO_ANSWER:
        elapsed = timeout_s
    report.latency.FromTimedelta(timedelta(seconds=elapsed))
    # A refused key's text can quote part of it.
    if report.status != 401:
        report.detail = _cut(_without(detail, got.api_key))
    return report


def _failure(e: BaseException) -> tuple[pb.ModelTestOutcome, int, str]:
    """What a failed request means: its outcome, the provider's HTTP status, and
    what the provider or the connection said."""
    if isinstance(e, ProviderError) and e.__cause__ is not None:
        e = e.__cause__
    match e:
        case httpx.HTTPStatusError():
            return pb.MODEL_TEST_OUTCOME_REFUSED, e.response.status_code, _said(e.response)
        case anthropic.APIStatusError():
            return pb.MODEL_TEST_OUTCOME_REFUSED, e.status_code, _message(e.body) or e.message
        case anthropic.APITimeoutError() | httpx.ReadTimeout() | httpx.WriteTimeout():
            return pb.MODEL_TEST_OUTCOME_NO_ANSWER, 0, ""
        case httpx.ConnectError() | httpx.ConnectTimeout():
            return pb.MODEL_TEST_OUTCOME_UNREACHABLE, 0, _plain(e)
        case anthropic.APIConnectionError():
            return pb.MODEL_TEST_OUTCOME_UNREACHABLE, 0, _plain(e.__cause__)
        case _:
            return pb.MODEL_TEST_OUTCOME_ERROR, 0, f"{type(e).__name__}: {e}"


def _said(resp: httpx.Response) -> str:
    """The error message in a response, or its text when it is short and plain."""
    try:
        return _message(resp.json())
    except ValueError:
        if "html" in resp.headers.get("content-type", ""):
            return ""
        return resp.text


def _message(body: object) -> str:
    """The message in an error body, in OpenAI's, Anthropic's, Ollama's or vLLM's
    form."""
    if not isinstance(body, dict):
        return ""
    fields = cast(dict[str, object], body)
    for key in ("error", "message", "detail"):
        value = fields.get(key)
        if isinstance(value, str):
            return value
        if found := _message(value):
            return found
    return ""


_ERRNO = re.compile(r"^\[Errno -?\d+\]\s*")


def _plain(e: BaseException | None) -> str:
    """A connection error as plain words, without its errno."""
    return _ERRNO.sub("", str(e or "")).strip()


def _without(text: str, key: str | None) -> str:
    return text.replace(key, "…") if key else text


def _cut(text: str) -> str:
    text = " ".join(text.split())
    return text if len(text) <= DETAIL_LIMIT else text[: DETAIL_LIMIT - 1] + "…"


class TestCore(Protocol):
    async def take_model_test(self) -> pb.ModelTest | None: ...

    async def report_model_test(self, req: pb.ReportModelTestRequest) -> None: ...


class ConnectionTester:
    """Takes each test the core hands out, runs it and reports it."""

    def __init__(
        self,
        core: TestCore,
        installed: Installed,
        template: Endpoint,
        connect: Connect = connect,
        retry_s: float = 5.0,
        min_wait_s: float = 1.0,
    ) -> None:
        self.core = core
        self.installed = installed
        self.template = template
        self.connect = connect
        self.retry_s = retry_s
        # A core that is stopping answers at once: wait this long between asks.
        self.min_wait_s = min_wait_s

    async def run(self) -> None:
        while True:
            started = time.monotonic()
            try:
                test = await self.core.take_model_test()
            except grpc.RpcError as e:
                if e.code() == grpc.StatusCode.UNIMPLEMENTED:
                    log.info("the core asks for no tests of the model's connection")
                    return
                # The worker says when the core is unavailable; this says nothing more.
                await asyncio.sleep(self.retry_s)
                continue
            if test is None:
                await asyncio.sleep(max(0.0, self.min_wait_s - (time.monotonic() - started)))
                continue
            await self.test(test)

    async def test(self, test: pb.ModelTest) -> None:
        try:
            report = await run_test(test, self.installed, self.template, self.connect)
        except Exception as e:
            report = pb.ReportModelTestRequest(
                id=test.id, outcome=pb.MODEL_TEST_OUTCOME_ERROR, detail=_cut(f"{type(e).__name__}")
            )
        try:
            await self.core.report_model_test(report)
        except grpc.RpcError as e:
            log.warning(
                "cannot report a test of the model's connection", extra={"code": e.code().name}
            )
