"""An adapter for OpenAI-compatible chat completions, as Ollama and others serve them."""

from typing import Any

import httpx
from pydantic import BaseModel

from themis_intelligence.platform.contract import ProviderRequest, ProviderResponse, Usage

# The caller's deadline bounds each call; this only backstops a hung socket.
TIMEOUT = httpx.Timeout(600)


class _Message(BaseModel):
    content: str | None = None


class _Choice(BaseModel):
    message: _Message


class _Usage(BaseModel):
    prompt_tokens: int = 0
    completion_tokens: int = 0


class _Completion(BaseModel):
    model: str = ""
    choices: list[_Choice]
    usage: _Usage | None = None


class OpenAICompatAdapter:
    def __init__(
        self,
        base_url: str,
        api_key: str | None = None,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.client = client or httpx.AsyncClient(timeout=TIMEOUT)

    async def send(self, req: ProviderRequest) -> ProviderResponse:
        headers = {"Authorization": f"Bearer {self.api_key}"} if self.api_key else {}
        body: dict[str, Any] = {
            "model": req.model,
            "messages": [m.model_dump() for m in req.messages],
            "max_tokens": req.max_output_tokens,
            "temperature": 0,
            "response_format": {
                "type": "json_schema",
                "json_schema": {"name": "reply", "schema": req.json_schema, "strict": True},
            },
        }
        resp = await self.client.post(
            f"{self.base_url}/chat/completions", json=body, headers=headers
        )
        resp.raise_for_status()
        data = _Completion.model_validate_json(resp.content)
        usage = data.usage or _Usage()
        return ProviderResponse(
            content=data.choices[0].message.content or "",
            usage=Usage(input_tokens=usage.prompt_tokens, output_tokens=usage.completion_tokens),
            model=data.model or req.model,
        )
