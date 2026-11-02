"""Model replies saved from a real run, so CI can replay an evaluation with no model.

Replies are keyed by everything the model was sent. When the prompt, the schema
or the evidence changes, the saved replies no longer match, and the replay
stops: a change like that needs new numbers. The same request can come up more
than once, in scenarios that differ only in evidence read later, and the model
needn't answer it the same way each time, so every reply is kept in order and
the replay hands them out in that order.
"""

import hashlib
import json
from pathlib import Path

from pydantic import BaseModel

from themis_intelligence.platform.client import Adapter
from themis_intelligence.platform.contract import ProviderRequest, ProviderResponse, Usage


class MissingRecordingError(Exception):
    """No reply was saved for this request."""


class Reply(BaseModel):
    content: str
    usage: Usage
    model: str
    # A reply cut off at the output limit replays as cut off, so the client
    # treats it as it did when it was recorded.
    truncated: bool = False


class Recording(BaseModel):
    replies: list[Reply]


def key(req: ProviderRequest) -> str:
    """The request's identity, whatever the model is called."""
    sent = {
        "messages": [m.model_dump() for m in req.messages],
        "schema": req.json_schema,
        "max_output_tokens": req.max_output_tokens,
    }
    return hashlib.sha256(json.dumps(sent, sort_keys=True).encode()).hexdigest()


class RecordingAdapter:
    """Passes requests to a real model and saves each reply in dir."""

    def __init__(self, adapter: Adapter, dir: Path) -> None:
        self.adapter = adapter
        self.dir = dir
        dir.mkdir(parents=True, exist_ok=True)

    async def send(self, req: ProviderRequest) -> ProviderResponse:
        resp = await self.adapter.send(req)
        path = self.dir / f"{key(req)}.json"
        saved = (
            Recording.model_validate_json(path.read_bytes())
            if path.exists()
            else Recording(replies=[])
        )
        saved.replies.append(
            Reply(
                content=resp.content, usage=resp.usage, model=resp.model, truncated=resp.truncated
            )
        )
        path.write_text(saved.model_dump_json(indent=2) + "\n")
        return resp


class ReplayAdapter:
    """Answers from the replies saved in dir, and never reaches a model."""

    def __init__(self, dir: Path) -> None:
        self.dir = dir
        # How many times each request has been answered so far.
        self.used: dict[str, int] = {}

    async def send(self, req: ProviderRequest) -> ProviderResponse:
        k = key(req)
        try:
            saved = Recording.model_validate_json((self.dir / f"{k}.json").read_bytes())
        except FileNotFoundError:
            raise MissingRecordingError(k) from None
        n = self.used.get(k, 0)
        if n >= len(saved.replies):
            raise MissingRecordingError(k)
        self.used[k] = n + 1
        reply = saved.replies[n]
        return ProviderResponse(
            content=reply.content, usage=reply.usage, model=reply.model, truncated=reply.truncated
        )
