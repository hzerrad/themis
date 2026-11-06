"""The Anthropic adapter, against a fake transport: nothing reaches Anthropic."""

import json
from collections.abc import Callable
from typing import Any

import anthropic
import httpx2
import pytest
from pydantic import BaseModel, Field

from tests.platform.test_client import request
from themis_intelligence.platform.adapters.anthropic_messages import PRICES, AnthropicAdapter
from themis_intelligence.platform.budget import Budget
from themis_intelligence.platform.client import Endpoint, ModelClient
from themis_intelligence.platform.contract import Message

MODEL = "claude-sonnet-5-5"


class Decision(BaseModel):
    verdict: str = Field(max_length=20)
    # A map keyed by name, which Anthropic's schema subset can't describe.
    params: dict[str, str] = Field(default_factory=dict[str, str])


def message(text: str, stop: str = "end_turn") -> dict[str, Any]:
    return {
        "id": "msg_1",
        "type": "message",
        "role": "assistant",
        "model": MODEL,
        "content": [
            {"type": "thinking", "thinking": "", "signature": "sig"},
            {"type": "text", "text": text},
        ],
        "stop_reason": stop,
        "stop_sequence": None,
        "usage": {"input_tokens": 1000, "output_tokens": 200},
    }


class Fake:
    """Anthropic's API as the SDK reaches it: answers each call in turn, and keeps what was sent."""

    def __init__(self, status: int, body: dict[str, Any]) -> None:
        self.status, self.body = status, body
        self.sent: list[dict[str, Any]] = []

    def __call__(self, req: httpx2.Request) -> httpx2.Response:
        self.sent.append(json.loads(req.content))
        return httpx2.Response(self.status, json=self.body)


def claude(fake: Callable[[httpx2.Request], httpx2.Response], effort: str | None) -> ModelClient:
    sdk = anthropic.AsyncAnthropic(
        api_key="test-key",
        base_url="http://claude.test",
        max_retries=0,
        http_client=anthropic.DefaultAsyncHttpxClient(transport=httpx2.MockTransport(fake)),
    )
    return ModelClient(
        adapter=AnthropicAdapter(api_key="test-key", client=sdk),
        endpoint=Endpoint(name="claude", model=MODEL, ceiling="sensitive", reasoning_effort=effort),
        budget=Budget(max_tokens=100_000),
    )


async def test_a_step_maps_to_a_messages_call_and_back() -> None:
    entries = [{"key": "pod", "value": "a"}]
    fake = Fake(200, message(json.dumps({"verdict": "rollout", "params": entries})))
    messages = [Message(role="system", content="be careful"), Message(role="user", content="why?")]
    result = await claude(fake, "medium").complete(request(messages=messages), Decision)

    assert result.error is None
    assert result.output == Decision(verdict="rollout", params={"pod": "a"})
    assert result.model == MODEL
    assert (result.usage.input_tokens, result.usage.output_tokens) == (1000, 200)
    assert PRICES[MODEL].cost(result.usage) == pytest.approx((1000 * 2 + 200 * 10) / 1e6)

    [sent] = fake.sent
    assert sent["system"] == "be careful"
    assert sent["messages"] == [{"role": "user", "content": "why?"}]
    assert sent["max_tokens"] == 100
    assert not {"temperature", "top_p", "top_k", "thinking"} & set(sent)
    assert sent["output_config"]["effort"] == "medium"
    schema = sent["output_config"]["format"]["schema"]
    assert schema["additionalProperties"] is False
    assert "maxLength" not in schema["properties"]["verdict"]
    # A map is a list of entries, so its values' schema still binds the model.
    assert schema["properties"]["params"]["items"]["properties"]["value"] == {"type": "string"}


@pytest.mark.parametrize(("stop", "kind"), [("refusal", "refused"), ("max_tokens", "truncated")])
async def test_a_reply_stopped_short_is_its_own_error(stop: str, kind: str) -> None:
    fake = Fake(200, message("I can't help with that.", stop))
    result = await claude(fake, None).complete(request(), Decision)
    assert result.error is not None and result.error.kind == kind
    assert len(fake.sent) == 1, "a repair would stop the same way"
    assert "effort" not in fake.sent[0]["output_config"]


@pytest.mark.parametrize(
    ("status", "effort", "calls"),
    [
        (400, "medium", 1),
        # Anthropic has no "none": nothing is sent.
        (200, "none", 0),
    ],
)
async def test_a_call_that_fails_is_unavailable(status: int, effort: str, calls: int) -> None:
    error = {"type": "error", "error": {"type": "invalid_request_error", "message": "bad"}}
    fake = Fake(status, error if status != 200 else message("{}"))
    result = await claude(fake, effort).complete(request(), Decision)
    assert result.error is not None and result.error.kind == "unavailable"
    assert "test-key" not in result.error.message
    assert len(fake.sent) == calls
