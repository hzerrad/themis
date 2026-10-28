import asyncio
import json

import httpx
import pytest
import respx
from pydantic import BaseModel

from themis_intelligence.platform.adapters.openai_compat import OpenAICompatAdapter
from themis_intelligence.platform.budget import Budget, BudgetExceededError
from themis_intelligence.platform.client import Endpoint, ModelClient
from themis_intelligence.platform.contract import Message, ModelRequest

BASE = "http://model.test/v1"


class Answer(BaseModel):
    verdict: str
    confidence: float


def reply(content: str, prompt: int = 10, completion: int = 5) -> httpx.Response:
    return httpx.Response(
        200,
        json={
            "model": "qwen3",
            "choices": [{"message": {"role": "assistant", "content": content}}],
            "usage": {"prompt_tokens": prompt, "completion_tokens": completion},
        },
    )


def request(**overrides: object) -> ModelRequest:
    fields: dict[str, object] = {
        "task_class": "investigation_step",
        "incident_id": "inc-1",
        "trace_id": "trace-1",
        "schema_version": "1",
        "data_classification": "internal",
        "prompt_version": "rollout_v1",
        "messages": [Message(role="user", content="is it the rollout?")],
        "deadline_s": 5.0,
        "max_output_tokens": 100,
    }
    fields.update(overrides)
    return ModelRequest.model_validate(fields)


def client(
    budget: Budget | None = None, ceiling: str = "sensitive", max_repairs: int = 1
) -> ModelClient:
    return ModelClient(
        adapter=OpenAICompatAdapter(base_url=BASE),
        endpoint=Endpoint(name="ollama", model="qwen3", ceiling=ceiling),  # type: ignore[arg-type]
        budget=budget or Budget(max_tokens=10_000),
        max_repairs=max_repairs,
    )


@respx.mock
async def test_valid_json_parsed_into_schema() -> None:
    respx.post(f"{BASE}/chat/completions").mock(
        return_value=reply('{"verdict": "rollout", "confidence": 0.9}')
    )
    result = await client().complete(request(), Answer)
    assert result.error is None
    assert result.output == Answer(verdict="rollout", confidence=0.9)


@respx.mock
async def test_schema_sent_to_the_model() -> None:
    route = respx.post(f"{BASE}/chat/completions").mock(
        return_value=reply('{"verdict": "x", "confidence": 1}')
    )
    await client().complete(request(), Answer)
    body = json.loads(route.calls.last.request.content)
    assert body["model"] == "qwen3"
    assert body["response_format"]["type"] == "json_schema"
    assert "verdict" in json.dumps(body["response_format"]["json_schema"]["schema"])


@respx.mock
async def test_malformed_json_repaired_once_then_typed_error() -> None:
    route = respx.post(f"{BASE}/chat/completions").mock(return_value=reply("not json"))
    result = await client(max_repairs=1).complete(request(), Answer)
    assert result.output is None
    assert result.error is not None and result.error.kind == "validation_failed"
    assert result.repair_attempts == 1
    assert route.call_count == 2


@respx.mock
async def test_repair_succeeds() -> None:
    respx.post(f"{BASE}/chat/completions").mock(
        side_effect=[reply("{}"), reply('{"verdict": "rollout", "confidence": 0.5}')]
    )
    result = await client().complete(request(), Answer)
    assert result.output is not None and result.repair_attempts == 1


@respx.mock
async def test_repair_charges_same_budget() -> None:
    respx.post(f"{BASE}/chat/completions").mock(
        side_effect=[reply("{}", 10, 5), reply('{"verdict": "x", "confidence": 1}', 20, 5)]
    )
    budget = Budget(max_tokens=10_000)
    result = await client(budget).complete(request(), Answer)
    assert result.usage.input_tokens == 30 and result.usage.output_tokens == 10
    assert budget.spent == 40


@respx.mock
async def test_reserve_before_dispatch_refuses_over_budget() -> None:
    route = respx.post(f"{BASE}/chat/completions").mock(return_value=reply("{}"))
    result = await client(Budget(max_tokens=50)).complete(request(max_output_tokens=100), Answer)
    assert result.error is not None and result.error.kind == "budget_exceeded"
    assert route.call_count == 0


@respx.mock
async def test_classification_above_endpoint_ceiling_rejected() -> None:
    route = respx.post(f"{BASE}/chat/completions").mock(return_value=reply("{}"))
    result = await client(ceiling="internal").complete(
        request(data_classification="sensitive"), Answer
    )
    assert result.error is not None and result.error.kind == "ineligible"
    assert route.call_count == 0


@respx.mock
async def test_deadline_cancels_request() -> None:
    async def slow(_: httpx.Request) -> httpx.Response:
        await asyncio.sleep(5)
        return reply("{}")

    respx.post(f"{BASE}/chat/completions").mock(side_effect=slow)
    result = await client().complete(request(deadline_s=0.05), Answer)
    assert result.error is not None and result.error.kind == "deadline_exceeded"


@respx.mock
async def test_unavailable_endpoint_is_a_typed_error() -> None:
    respx.post(f"{BASE}/chat/completions").mock(return_value=httpx.Response(503))
    result = await client().complete(request(), Answer)
    assert result.error is not None and result.error.kind == "unavailable"


@respx.mock
async def test_result_records_provenance_fields() -> None:
    respx.post(f"{BASE}/chat/completions").mock(
        return_value=reply('{"verdict": "x", "confidence": 1}')
    )
    result = await client().complete(request(), Answer)
    assert result.model == "qwen3"
    assert result.endpoint == "ollama"
    assert result.route == "default"
    assert result.prompt_version == "rollout_v1"
    assert result.schema_version == "1"
    assert result.latency_ms >= 0


def test_budget_settles_to_actual_usage() -> None:
    budget = Budget(max_tokens=100)
    r = budget.reserve(80)
    with pytest.raises(BudgetExceededError):
        budget.reserve(30)
    budget.settle(r, 20)
    assert budget.spent == 20
    budget.reserve(70)
