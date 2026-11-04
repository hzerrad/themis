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
async def test_malformed_json_repaired_once_then_typed_error() -> None:
    route = respx.post(f"{BASE}/chat/completions").mock(return_value=reply("not json"))
    result = await client(max_repairs=1).complete(request(), Answer)
    assert result.output is None
    assert result.error is not None and result.error.kind == "validation_failed"
    assert result.repair_attempts == 1
    assert route.call_count == 2


# A repaired reply is parsed, and the repair is charged to the same budget.
@respx.mock
async def test_repair_charges_same_budget() -> None:
    respx.post(f"{BASE}/chat/completions").mock(
        side_effect=[reply("{}", 10, 5), reply('{"verdict": "x", "confidence": 1}', 20, 5)]
    )
    budget = Budget(max_tokens=10_000)
    result = await client(budget).complete(request(), Answer)
    assert result.output is not None and result.repair_attempts == 1
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


# A valid reply is parsed into the schema, with where it came from.
@respx.mock
async def test_result_records_provenance_fields() -> None:
    respx.post(f"{BASE}/chat/completions").mock(
        return_value=reply('{"verdict": "rollout", "confidence": 0.9}')
    )
    result = await client().complete(request(), Answer)
    assert result.error is None
    assert result.output == Answer(verdict="rollout", confidence=0.9)
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


@respx.mock
async def test_reasoning_effort_and_output_limit_reach_the_model_only_when_set() -> None:
    route = respx.post(f"{BASE}/chat/completions").mock(
        return_value=reply('{"verdict": "x", "confidence": 1}')
    )
    await client().complete(request(), Answer)
    body = json.loads(route.calls.last.request.content)
    assert "reasoning_effort" not in body
    assert body["max_tokens"] == 100

    tuned = ModelClient(
        adapter=OpenAICompatAdapter(base_url=BASE),
        endpoint=Endpoint(
            name="ollama",
            model="qwen3",
            ceiling="sensitive",
            reasoning_effort="none",
            max_output_tokens=4096,
        ),
        budget=Budget(max_tokens=10_000),
    )
    await tuned.complete(request(), Answer)
    body = json.loads(route.calls.last.request.content)
    assert body["reasoning_effort"] == "none"
    assert body["max_tokens"] == 4096


# A model that spends its output limit thinking replies with nothing: that is
# a cut-off reply, not a malformed one, and a repair would be cut off too.
@respx.mock
async def test_a_reply_cut_off_at_the_limit_is_its_own_error() -> None:
    route = respx.post(f"{BASE}/chat/completions").mock(
        return_value=httpx.Response(
            200,
            json={
                "model": "qwen3",
                "choices": [
                    {"message": {"role": "assistant", "content": ""}, "finish_reason": "length"}
                ],
                "usage": {"prompt_tokens": 10, "completion_tokens": 100},
            },
        )
    )
    result = await client().complete(request(), Answer)
    assert result.error is not None
    assert result.error.kind == "truncated"
    assert "cut off at 100 tokens" in result.error.message
    assert route.call_count == 1
    assert result.usage.output_tokens == 100


# The model gets the schema, strictly unless the endpoint turns that off.
@respx.mock
async def test_strict_output_is_on_unless_the_endpoint_turns_it_off() -> None:
    route = respx.post(f"{BASE}/chat/completions").mock(
        return_value=reply('{"verdict": "x", "confidence": 1}')
    )
    await client().complete(request(), Answer)
    body = json.loads(route.calls.last.request.content)
    assert body["model"] == "qwen3"
    assert body["response_format"]["type"] == "json_schema"
    assert "verdict" in json.dumps(body["response_format"]["json_schema"]["schema"])
    assert body["response_format"]["json_schema"]["strict"]

    loose = ModelClient(
        adapter=OpenAICompatAdapter(base_url=BASE),
        endpoint=Endpoint(name="openai", model="m", ceiling="sensitive", strict_output=False),
        budget=Budget(max_tokens=10_000),
    )
    result = await loose.complete(request(), Answer)
    assert result.error is None
    assert not json.loads(route.calls.last.request.content)["response_format"]["json_schema"][
        "strict"
    ]
