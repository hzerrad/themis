"""Asks HolmesGPT to investigate, through its HTTP API's /api/chat."""

from dataclasses import dataclass
from typing import Any

import httpx
from pydantic import BaseModel, ValidationError

# HolmesGPT runs its own tools and model; one investigation takes minutes.
TIMEOUT = httpx.Timeout(600.0, connect=10.0)


# At most this many tool calls are listed in the notes.
MAX_LISTED = 40


class _Result(BaseModel):
    status: str = ""


class _ToolCall(BaseModel):
    tool_name: str = ""
    description: str = ""
    result: _Result | None = None


class _Reply(BaseModel):
    analysis: str
    tool_calls: list[_ToolCall] | None = None
    conversation_history: list[dict[str, Any]] | None = None


@dataclass(frozen=True)
class Answer:
    analysis: str
    # tools_run is how many tools HolmesGPT ran to answer.
    tools_run: int
    # notes are for people: what HolmesGPT ran, never the output, which can
    # hold logs and secrets, and what it found, in its own words.
    notes: str


# HolmesGPT's own findings are shortened to this in the notes.
MAX_FINDINGS = 4000


def notes(calls: list[_ToolCall], findings: str) -> str:
    parts = [ran(calls)]
    findings = findings.strip()
    if findings:
        if len(findings) > MAX_FINDINGS:
            findings = findings[:MAX_FINDINGS] + " …(shortened)"
        parts.append("What HolmesGPT found:\n" + findings)
    return "\n\n".join(parts)


def ran(calls: list[_ToolCall]) -> str:
    if not calls:
        return "HolmesGPT ran no tools."
    lines: list[str] = []
    for c in calls[:MAX_LISTED]:
        what = c.description.strip()
        if not what or what == "NA":
            what = c.tool_name or "a tool"
        status = c.result.status if c.result and c.result.status else ""
        lines.append(f"- {what}{f' ({status})' if status else ''}")
    if len(calls) > MAX_LISTED:
        lines.append(f"- and {len(calls) - MAX_LISTED} more")
    return f"HolmesGPT ran {len(calls)} tool{'s' if len(calls) != 1 else ''}:\n" + "\n".join(lines)


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

    async def ask(
        self, ask: str, instructions: str, conclude: str, schema: dict[str, Any]
    ) -> "Answer":
        """Asks in two turns: first to investigate, free to run tools, then,
        in the same conversation, for the verdict as JSON in schema. Asked
        for JSON from the start, a model can't call a tool first."""
        first = await self._chat({"ask": ask, "additional_system_prompt": instructions})
        if not first.conversation_history:
            raise HolmesError("HolmesGPT returned no conversation to continue")
        second = await self._chat(
            {
                "ask": conclude,
                "conversation_history": first.conversation_history,
                "response_format": {
                    "type": "json_schema",
                    "json_schema": {"name": "themis_suggestion", "strict": True, "schema": schema},
                },
            }
        )
        calls = (first.tool_calls or []) + (second.tool_calls or [])
        return Answer(second.analysis, len(calls), notes(calls, first.analysis))

    async def _chat(self, body: dict[str, Any]) -> "_Reply":
        body = body | {"stream": False}
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
            return _Reply.model_validate(resp.json())
        except (ValueError, ValidationError) as e:
            raise HolmesError("HolmesGPT's reply has no analysis") from e

    async def close(self) -> None:
        await self.client.aclose()
