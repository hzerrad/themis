from collections.abc import Callable
from pathlib import Path

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


def models(adapter: Adapter) -> Callable[[int], Model]:
    def new_model(tokens: int) -> Model:
        return ModelClient(adapter=adapter, endpoint=ENDPOINT, budget=Budget(tokens))

    return new_model


def saved(d: Path) -> int:
    return len(list(d.glob("*.json")))


async def test_a_replay_gives_the_recorded_run(tmp_path: Path) -> None:
    live = ScriptedAdapter(LOGS, propose())
    recorded = await run_all([oom()], models(RecordingAdapter(live, tmp_path)), 1, CAPS)
    assert live.calls == 2
    assert saved(tmp_path) == 2

    replayed = await run_all([oom()], models(ReplayAdapter(tmp_path)), 1, CAPS)
    a, b = recorded.records[0], replayed.records[0]
    assert (b.conclusion, b.proposal, b.decisions) == (a.conclusion, a.proposal, a.decisions)
    assert (b.input_tokens, b.output_tokens) == (a.input_tokens, a.output_tokens)


async def test_a_changed_prompt_misses_the_recordings(tmp_path: Path) -> None:
    live = RecordingAdapter(ScriptedAdapter(LOGS, propose()), tmp_path)
    await run_all([oom()], models(live), 1, CAPS)
    changed = oom()
    changed.predicates[0].reason = "a different reason"
    with pytest.raises(MissingRecordingError):
        await run_all([changed], models(ReplayAdapter(tmp_path)), 1, CAPS)


async def test_the_same_request_answered_differently_replays_in_order(tmp_path: Path) -> None:
    # Two runs of one scenario send the same first request; the model answers
    # it differently each time.
    live = ScriptedAdapter(DECLINE, LOGS, DECLINE, LOGS, propose())
    recorded = await run_all([oom()], models(RecordingAdapter(live, tmp_path)), 2, CAPS)
    assert [r.conclusion for r in recorded.records] == ["abstained", "proposed"]

    replayed = await run_all([oom()], models(ReplayAdapter(tmp_path)), 2, CAPS)
    assert [r.conclusion for r in replayed.records] == ["abstained", "proposed"]


async def test_a_request_asked_more_often_than_recorded_misses(tmp_path: Path) -> None:
    await run_all(
        [oom()], models(RecordingAdapter(ScriptedAdapter(LOGS, propose()), tmp_path)), 1, CAPS
    )
    with pytest.raises(MissingRecordingError):
        await run_all([oom()], models(ReplayAdapter(tmp_path)), 2, CAPS)


class Truncating(ScriptedAdapter):
    """Cuts off its first reply, as a model that thinks too long would be."""

    async def send(self, req: ProviderRequest) -> ProviderResponse:
        resp = await super().send(req)
        if self.calls == 1:
            return resp.model_copy(update={"content": "", "truncated": True})
        return resp


async def test_a_cut_off_reply_replays_as_cut_off(tmp_path: Path) -> None:
    live = Truncating(LOGS, LOGS, propose())
    recorded = await run_all([oom()], models(RecordingAdapter(live, tmp_path)), 1, CAPS)
    replayed = await run_all([oom()], models(ReplayAdapter(tmp_path)), 1, CAPS)
    a, b = recorded.records[0], replayed.records[0]
    assert (b.conclusion, b.decisions, b.clean_calls) == (a.conclusion, a.decisions, a.clean_calls)
