from collections.abc import Callable
from functools import cache
from pathlib import Path
from typing import Any

import pytest

from tests.evaluation.helpers import DECLINE, LOGS, ScriptedAdapter, oom, propose
from tests.evaluation.test_runner import CAPS
from themis_intelligence.evaluation.recordings import (
    MissingRecordingError,
    RecordingAdapter,
    ReplayAdapter,
)
from themis_intelligence.evaluation.runner import run_all
from themis_intelligence.investigation.loop import Model
from themis_intelligence.platform.budget import Budget
from themis_intelligence.platform.client import Adapter, Endpoint, ModelClient
from themis_intelligence.platform.contract import ProviderRequest, ProviderResponse

ENDPOINT = Endpoint(name="eval", model="m", ceiling="sensitive")


def models(adapter_for: Callable[[str], Adapter]) -> Callable[[str, int], Model]:
    """A model per run, whose adapter is kept across a case's runs, as the evaluation does."""
    adapter_for = cache(adapter_for)

    def new_model(case: str, tokens: int) -> Model:
        return ModelClient(adapter=adapter_for(case), endpoint=ENDPOINT, budget=Budget(tokens))

    return new_model


def recording(live: Adapter, d: Path) -> Callable[[str, int], Model]:
    return models(lambda case: RecordingAdapter(live, d, case))


def replaying(d: Path) -> Callable[[str, int], Model]:
    return models(lambda case: ReplayAdapter(d, case))


def saved(d: Path) -> int:
    return len(list(d.rglob("*.json")))


async def test_a_replay_gives_the_recorded_run(tmp_path: Path) -> None:
    live = ScriptedAdapter(LOGS, propose())
    recorded = await run_all([oom()], recording(live, tmp_path), 1, CAPS)
    assert live.calls == 2
    assert saved(tmp_path) == 2

    replayed = await run_all([oom()], replaying(tmp_path), 1, CAPS)
    a, b = recorded.records[0], replayed.records[0]
    assert (b.conclusion, b.proposal, b.decisions) == (a.conclusion, a.proposal, a.decisions)
    assert (b.input_tokens, b.output_tokens) == (a.input_tokens, a.output_tokens)


async def test_a_changed_prompt_misses_the_recordings(tmp_path: Path) -> None:
    await run_all([oom()], recording(ScriptedAdapter(LOGS, propose()), tmp_path), 1, CAPS)
    changed = oom()
    changed.predicates[0].reason = "a different reason"
    with pytest.raises(MissingRecordingError):
        await run_all([changed], replaying(tmp_path), 1, CAPS)


async def test_the_same_request_answered_differently_replays_in_order(tmp_path: Path) -> None:
    # Two runs of one scenario send the same first request; the model answers
    # it differently each time.
    live = ScriptedAdapter(DECLINE, LOGS, DECLINE, LOGS, propose())
    recorded = await run_all([oom()], recording(live, tmp_path), 2, CAPS)
    assert [r.conclusion for r in recorded.records] == ["abstained", "proposed"]

    replayed = await run_all([oom()], replaying(tmp_path), 2, CAPS)
    assert [r.conclusion for r in replayed.records] == ["abstained", "proposed"]


async def test_cases_sending_the_same_request_replay_their_own_replies(tmp_path: Path) -> None:
    # A twin whose difference lies in evidence read later sends its original's
    # requests; replayed in the other order, each still gets its own replies.
    twin = oom().model_copy(update={"scenario": "rollout-oom-twin"})
    live = ScriptedAdapter(LOGS, propose(), LOGS, DECLINE)
    recorded = await run_all([oom(), twin], recording(live, tmp_path), 1, CAPS)
    assert [r.conclusion for r in recorded.records] == ["proposed", "abstained"]

    replayed = await run_all([twin, oom()], replaying(tmp_path), 1, CAPS)
    assert [r.conclusion for r in replayed.records] == ["abstained", "proposed"]


async def test_a_request_asked_more_often_than_recorded_misses(tmp_path: Path) -> None:
    await run_all([oom()], recording(ScriptedAdapter(LOGS, propose()), tmp_path), 1, CAPS)
    with pytest.raises(MissingRecordingError):
        await run_all([oom()], replaying(tmp_path), 2, CAPS)


class Stopping(ScriptedAdapter):
    """Stops its first reply short: cut off, as a model that thinks too long
    would be, or refused."""

    def __init__(self, how: str, *steps: Any) -> None:
        super().__init__(*steps)
        self.how = how

    async def send(self, req: ProviderRequest) -> ProviderResponse:
        resp = await super().send(req)
        if self.calls == 1:
            return resp.model_copy(update={"content": "", self.how: True})
        return resp


@pytest.mark.parametrize("how", ["truncated", "refused"])
async def test_a_reply_stopped_short_replays_as_stopped(tmp_path: Path, how: str) -> None:
    live = Stopping(how, LOGS, LOGS, propose())
    recorded = await run_all([oom()], recording(live, tmp_path), 1, CAPS)
    replayed = await run_all([oom()], replaying(tmp_path), 1, CAPS)
    a, b = recorded.records[0], replayed.records[0]
    assert (b.conclusion, b.decisions, b.clean_calls) == (a.conclusion, a.decisions, a.clean_calls)
