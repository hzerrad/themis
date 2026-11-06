"""An adapter for Anthropic's Messages API, through Anthropic's own SDK.

The reply comes as structured output, in the schema subset Anthropic accepts
(see schema_translation). Thinking is left to the model, which decides how much
it needs; the reasoning effort, when set, says how hard it tries. Sampling
settings aren't sent: these models refuse any but their own.
"""

from typing import Literal, cast, get_args

import anthropic
from anthropic.types import MessageParam, OutputConfigParam

from themis_intelligence.platform.adapters.schema_translation import ANTHROPIC, translate
from themis_intelligence.platform.client import Price
from themis_intelligence.platform.contract import (
    ProviderError,
    ProviderRequest,
    ProviderResponse,
    Usage,
)

# The caller's deadline bounds each call. Connecting is bounded separately, so
# an unreachable endpoint reads as unavailable rather than slow. The SDK retries
# what is worth retrying, such as rate limits, on its own.
TIMEOUT = anthropic.Timeout(600.0, connect=10.0)

Effort = Literal["low", "medium", "high", "xhigh", "max"]
EFFORTS: tuple[str, ...] = get_args(Effort)

# US dollars per million tokens, for prompts up to 100K tokens; Themis's are
# far smaller.
PRICES: dict[str, Price] = {
    "claude-sonnet-5-5": Price(2.00, 10.00),
    "claude-haiku-5-5": Price(0.10, 0.50),
}

# The reply ran into a limit before it was complete.
_CUT_OFF = ("max_tokens", "model_context_window_exceeded")


class AnthropicAdapter:
    def __init__(
        self,
        api_key: str | None,
        base_url: str | None = None,
        client: anthropic.AsyncAnthropic | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/") if base_url else None
        self.api_key = api_key
        self.client = client or anthropic.AsyncAnthropic(
            api_key=api_key, base_url=self.base_url, timeout=TIMEOUT
        )

    async def aclose(self) -> None:
        await self.client.close()

    async def send(self, req: ProviderRequest) -> ProviderResponse:
        schema = translate(req.json_schema, ANTHROPIC)
        config: OutputConfigParam = {"format": {"type": "json_schema", "schema": schema.sent}}
        if req.reasoning_effort:
            if req.reasoning_effort not in EFFORTS:
                raise ProviderError(
                    f"the reasoning effort must be one of {', '.join(EFFORTS)},"
                    f" not {req.reasoning_effort}"
                )
            config["effort"] = cast(Effort, req.reasoning_effort)
        system = "\n\n".join(m.content for m in req.messages if m.role == "system")
        messages: list[MessageParam] = [
            {"role": "assistant", "content": m.content}
            if m.role == "assistant"
            else {"role": "user", "content": m.content}
            for m in req.messages
            if m.role != "system"
        ]
        try:
            msg = await self.client.messages.create(
                model=req.model,
                max_tokens=req.max_output_tokens,
                messages=messages,
                output_config=config,
                system=system or anthropic.omit,
            )
        except anthropic.APIError as e:
            raise ProviderError(f"{type(e).__name__}: {e.message}") from e
        u = msg.usage
        usage = Usage(
            input_tokens=u.input_tokens
            + (u.cache_creation_input_tokens or 0)
            + (u.cache_read_input_tokens or 0),
            # Thinking is billed as output, and counted in it.
            output_tokens=u.output_tokens,
        )
        text = "".join(b.text for b in msg.content if b.type == "text")
        truncated = msg.stop_reason in _CUT_OFF
        refused = msg.stop_reason == "refusal"
        return ProviderResponse(
            content=text if truncated or refused else schema.decode(text),
            usage=usage,
            model=msg.model or req.model,
            truncated=truncated,
            refused=refused,
        )
