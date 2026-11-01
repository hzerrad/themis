"""The model client: eligibility, budget, deadline and schema validation around an adapter."""

import asyncio
import time
from dataclasses import dataclass
from typing import Protocol

import httpx
from opentelemetry.trace import SpanKind, Status, StatusCode
from pydantic import BaseModel, ValidationError

from themis_intelligence import telemetry, tracing
from themis_intelligence.platform.budget import Budget, BudgetExceededError
from themis_intelligence.platform.contract import (
    CLASSIFICATION_RANK,
    Classification,
    ErrorKind,
    Message,
    ModelRequest,
    ModelResult,
    ProviderRequest,
    ProviderResponse,
    TypedError,
    Usage,
)


class Adapter(Protocol):
    async def send(self, req: ProviderRequest) -> ProviderResponse: ...


@dataclass(frozen=True)
class Endpoint:
    name: str
    model: str
    # ceiling is the most sensitive data the endpoint may receive.
    ceiling: Classification


@dataclass(frozen=True)
class Price:
    """A route's price, in US dollars per million tokens."""

    input_usd_per_mtok: float
    output_usd_per_mtok: float

    def cost(self, usage: Usage) -> float:
        return (
            usage.input_tokens * self.input_usd_per_mtok
            + usage.output_tokens * self.output_usd_per_mtok
        ) / 1_000_000


def estimate_tokens(messages: list[Message]) -> int:
    """A rough input estimate: about four characters per token."""
    return sum(len(m.content) for m in messages) // 4 + 1


class ModelClient:
    def __init__(
        self,
        adapter: Adapter,
        endpoint: Endpoint,
        budget: Budget,
        max_repairs: int = 1,
        route: str = "default",
        price: Price | None = None,
    ) -> None:
        self.adapter = adapter
        self.endpoint = endpoint
        self.budget = budget
        self.max_repairs = max_repairs
        self.route = route
        self.price = price

    @property
    def ceiling(self) -> Classification:
        return self.endpoint.ceiling

    async def complete[T: BaseModel](self, req: ModelRequest, schema: type[T]) -> ModelResult[T]:
        started = time.monotonic()
        route = telemetry.route_label(self.route)
        with tracing.tracer.start_as_current_span("model.request", kind=SpanKind.CLIENT) as span:
            result = await self._complete(req, schema)
            outcome = result.error.kind if result.error else "ok"
            # Never the prompt or the reply: either can quote evidence.
            span.set_attributes(
                {
                    "themis.model.route": route,
                    "themis.model.outcome": outcome,
                    "gen_ai.request.model": self.endpoint.model,
                    "gen_ai.usage.input_tokens": result.usage.input_tokens,
                    "gen_ai.usage.output_tokens": result.usage.output_tokens,
                }
            )
            if result.error:
                span.set_status(Status(StatusCode.ERROR, outcome))
        telemetry.model_requests.labels(route, outcome).inc()
        telemetry.model_request_duration.labels(route).observe(time.monotonic() - started)
        telemetry.model_tokens.labels(route, "input").inc(result.usage.input_tokens)
        telemetry.model_tokens.labels(route, "output").inc(result.usage.output_tokens)
        if self.price is not None:
            telemetry.model_cost.labels(route).inc(self.price.cost(result.usage))
        return result

    async def _complete[T: BaseModel](self, req: ModelRequest, schema: type[T]) -> ModelResult[T]:
        result = ModelResult[T](
            model=self.endpoint.model,
            endpoint=self.endpoint.name,
            route=self.route,
            prompt_version=req.prompt_version,
            schema_version=req.schema_version,
        )

        def fail(kind: ErrorKind, message: str) -> ModelResult[T]:
            result.error = TypedError(kind=kind, message=message)
            return result

        if (
            CLASSIFICATION_RANK[req.data_classification]
            > CLASSIFICATION_RANK[self.endpoint.ceiling]
        ):
            return fail(
                "ineligible",
                f"{req.data_classification} data may not go to {self.endpoint.name}",
            )

        messages = list(req.messages)
        started = time.monotonic()
        try:
            async with asyncio.timeout(req.deadline_s):
                while True:
                    try:
                        reservation = self.budget.reserve(
                            estimate_tokens(messages) + req.max_output_tokens
                        )
                    except BudgetExceededError as e:
                        return fail("budget_exceeded", str(e))
                    used = Usage()
                    try:
                        resp = await self.adapter.send(
                            ProviderRequest(
                                model=self.endpoint.model,
                                messages=messages,
                                max_output_tokens=req.max_output_tokens,
                                json_schema=schema.model_json_schema(),
                            )
                        )
                        used = resp.usage
                    finally:
                        self.budget.settle(reservation, used.input_tokens + used.output_tokens)
                    result.usage = result.usage + used
                    result.model = resp.model or result.model
                    try:
                        result.output = schema.model_validate_json(resp.content)
                        return result
                    except ValidationError as e:
                        telemetry.model_validation_failures.labels(
                            telemetry.route_label(self.route)
                        ).inc()
                        if not req.allow_repair or result.repair_attempts >= self.max_repairs:
                            return fail("validation_failed", str(e))
                        result.repair_attempts += 1
                        messages = [
                            *messages,
                            Message(role="assistant", content=resp.content),
                            Message(
                                role="user",
                                content="Your reply did not match the required JSON schema: "
                                f"{e.errors(include_url=False)}. Reply again with only JSON "
                                "that matches the schema.",
                            ),
                        ]
        except TimeoutError:
            return fail("deadline_exceeded", f"no valid reply within {req.deadline_s}s")
        except httpx.HTTPError as e:
            return fail("unavailable", f"{type(e).__name__}: {e}")
        finally:
            result.latency_ms = int((time.monotonic() - started) * 1000)
