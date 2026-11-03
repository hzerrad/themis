"""Asks HolmesGPT to investigate, through its HTTP API's /api/chat."""

from typing import Any

import httpx

# HolmesGPT runs its own tools and model; one investigation takes minutes.
TIMEOUT = httpx.Timeout(600.0, connect=10.0)


class HolmesError(Exception):
    """HolmesGPT couldn't answer."""


class Holmes:
    def __init__(
        self,
        base_url: str,
        api_key: str | None,
        model: str | None,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        # A key of HolmesGPT's modelList; None leaves its default.
        self.model = model
        self.client = client or httpx.AsyncClient(timeout=TIMEOUT)

    def _headers(self) -> dict[str, str]:
        return {"X-API-Key": self.api_key} if self.api_key else {}

    async def ask(self, ask: str, instructions: str, schema: dict[str, Any]) -> str:
        """Returns HolmesGPT's answer: JSON in schema, if its model followed it."""
        body: dict[str, Any] = {
            "ask": ask,
            "additional_system_prompt": instructions,
            "response_format": {
                "type": "json_schema",
                "json_schema": {"name": "themis_suggestion", "strict": True, "schema": schema},
            },
            "stream": False,
        }
        if self.model:
            body["model"] = self.model
        try:
            resp = await self.client.post(
                f"{self.base_url}/api/chat", json=body, headers=self._headers()
            )
        except httpx.HTTPError as e:
            raise HolmesError(f"HolmesGPT can't be reached at {self.base_url}: {e}") from e
        if resp.status_code in (401, 403):
            raise HolmesError("HolmesGPT refused the API key")
        if resp.status_code == 404:
            raise HolmesError(
                "HolmesGPT has no /api/chat: it's older than this adapter supports (0.25)"
            )
        if resp.status_code >= 400:
            raise HolmesError(f"HolmesGPT answered {resp.status_code}: {resp.text[:300]}")
        try:
            analysis = resp.json()["analysis"]
        except (ValueError, KeyError, TypeError) as e:
            raise HolmesError("HolmesGPT's reply has no analysis") from e
        if not isinstance(analysis, str):
            raise HolmesError("HolmesGPT's analysis isn't text")
        return analysis

    async def close(self) -> None:
        await self.client.aclose()
