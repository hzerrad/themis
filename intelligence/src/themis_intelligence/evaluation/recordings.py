"""Model replies saved from a real run, so CI can replay an evaluation with no model.

Each case's replies are kept apart, in a folder named after it, and keyed by
everything the model was sent. When the prompt, the schema or the evidence
changes, the saved replies no longer match, and the replay stops: a change like
that needs new numbers. Two cases that differ only in evidence read later send
the same first request, which is why the case is part of the key. Within a case
the same request comes up once per run, and the model needn't answer it the
same way each time, so every reply is kept in order and the replay hands them
out in that order.
"""

import hashlib
import json
import shutil
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
    # A refusal replays as a refusal.
    refused: bool = False


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
    """Passes one case's requests to a real model and saves each reply in dir.

    The case's earlier replies are dropped, so a recording replaces them.
    """

    def __init__(self, adapter: Adapter, dir: Path, case: str) -> None:
        self.adapter = adapter
        self.dir = dir / case
        shutil.rmtree(self.dir, ignore_errors=True)
        self.dir.mkdir(parents=True)

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
                content=resp.content,
                usage=resp.usage,
                model=resp.model,
                truncated=resp.truncated,
                refused=resp.refused,
            )
        )
        path.write_text(saved.model_dump_json(indent=2) + "\n")
        return resp


class ReplayAdapter:
    """Answers one case's requests from the replies saved in dir, and never reaches a model."""

    def __init__(self, dir: Path, case: str) -> None:
        self.dir = dir / case
        self.case = case
        # How many times each request has been answered so far.
        self.used: dict[str, int] = {}

    async def send(self, req: ProviderRequest) -> ProviderResponse:
        k = key(req)
        try:
            saved = Recording.model_validate_json((self.dir / f"{k}.json").read_bytes())
        except FileNotFoundError:
            raise MissingRecordingError(f"{self.case}/{k}") from None
        n = self.used.get(k, 0)
        if n >= len(saved.replies):
            raise MissingRecordingError(f"{self.case}/{k}")
        self.used[k] = n + 1
        reply = saved.replies[n]
        return ProviderResponse(
            content=reply.content,
            usage=reply.usage,
            model=reply.model,
            truncated=reply.truncated,
            refused=reply.refused,
        )
