from typing import Any

from tests.evaluation.helpers import DECLINE, DOWN, LOGS, TARGET, ScriptedAdapter, oom, propose
from themis_intelligence.evaluation.bundle import Bundle
from themis_intelligence.evaluation.runner import INVESTIGATION_TOKENS, Caps, Spend, run_all
from themis_intelligence.investigation.loop import Model
from themis_intelligence.platform.budget import Budget
from themis_intelligence.platform.client import Endpoint, ModelClient, Price
from themis_intelligence.platform.contract import Classification, Usage

CAPS = Caps(max_tokens=10_000_000)


def client(adapter: ScriptedAdapter, ceiling: Classification = "sensitive") -> Any:
    endpoint = Endpoint(name="eval", model="asked-model", ceiling=ceiling)

    def new_model(tokens: int) -> Model:
        return ModelClient(adapter=adapter, endpoint=endpoint, budget=Budget(tokens))

    return new_model


async def run(
    adapter: ScriptedAdapter, bundles: list[Bundle] | None = None, runs: int = 1, caps: Caps = CAPS
) -> Any:
    return await run_all(bundles or [oom()], client(adapter), runs, caps)


async def test_a_right_answer_runs_the_real_loop() -> None:
    out = await run(
        ScriptedAdapter(
            LOGS, propose(cited_evidence_ids=["ev-container_logs_tail", "ev-pod_states"])
        )
    )
    r = out.records[0]
    assert out.stopped == ""
    assert r.conclusion == "proposed"
    assert r.proposal.revision == 1
    assert r.proposal.target.uid == TARGET["uid"]
    assert "ev-container_logs_tail" in r.shown
    assert r.hypotheses[0].root_cause_class == "memory_limit_regression"
    assert (r.steps, r.calls, r.clean_calls) == (2, 2, 2)
    assert (r.input_tokens, r.output_tokens) == (200, 200)
    assert r.served_by == "served-model"


async def test_the_loop_holds_a_proposal_until_logs_are_read() -> None:
    adapter = ScriptedAdapter(propose(), LOGS, propose())
    r = (await run(adapter)).records[0]
    assert r.conclusion == "proposed"
    assert r.steps == 3


async def test_a_wrong_target_reaches_the_scorer_unchanged() -> None:
    other = {**TARGET, "namespace": "kube-system", "name": "coredns"}
    r = (await run(ScriptedAdapter(LOGS, propose(target=other)))).records[0]
    assert r.proposal.target.name == "coredns"


async def test_declining_after_the_nudge() -> None:
    r = (await run(ScriptedAdapter(DECLINE, LOGS, DECLINE))).records[0]
    assert r.conclusion == "abstained"
    assert r.steps == 3


async def test_proposing_past_the_hold_until_steps_run_out_is_not_declining() -> None:
    r = (await run(ScriptedAdapter(propose()))).records[0]
    assert r.conclusion == "out_of_steps"
    assert r.proposal is None
    assert r.decisions == ["propose"] * oom().max_steps


async def test_a_repaired_reply_is_counted() -> None:
    r = (await run(ScriptedAdapter("not json", LOGS, propose()))).records[0]
    assert r.conclusion == "proposed"
    assert (r.calls, r.clean_calls, r.repaired_calls) == (2, 1, 1)


async def test_invalid_output_is_the_models_failure() -> None:
    r = (await run(ScriptedAdapter("not json"))).records[0]
    assert r.conclusion == "failed"
    assert r.failure.startswith("invalid_model_output")


async def test_an_outage_stops_the_evaluation_and_is_not_scored_against_the_model() -> None:
    out = await run(ScriptedAdapter(DOWN), runs=10)
    assert len(out.records) == 3
    assert all(r.conclusion == "errored" for r in out.records)
    assert "errors in a row" in out.stopped


async def test_a_single_outage_does_not_stop_it() -> None:
    # The first run's only call fails; the rest follow the script.
    out = await run(ScriptedAdapter(DOWN, LOGS, DECLINE, DECLINE), runs=2)
    assert out.stopped == ""
    assert out.records[0].conclusion == "errored"


async def test_the_token_cap_stops_before_passing_it() -> None:
    adapter = ScriptedAdapter(LOGS, propose(), tokens=3000)
    out = await run(adapter, runs=100, caps=Caps(max_tokens=30_000))
    assert "spending cap" in out.stopped
    assert 0 < len(out.records) < 100
    spent = adapter.calls * 6000
    assert spent <= 30_000


async def test_the_dollar_cap_stops_before_passing_it() -> None:
    adapter = ScriptedAdapter(LOGS, propose(), tokens=3000)
    price = Price(1.0, 2.0)
    caps = Caps(max_tokens=10_000_000, max_usd=0.05, price=price)
    out = await run(adapter, runs=100, caps=caps)
    assert "spending cap" in out.stopped
    assert sum(r.cost_usd for r in out.records) <= 0.05


async def test_the_run_cut_off_by_the_cap_is_dropped() -> None:
    adapter = ScriptedAdapter(LOGS, propose(), tokens=3000)
    out = await run(adapter, runs=100, caps=Caps(max_tokens=30_000))
    assert all(r.conclusion == "proposed" for r in out.records)


def test_allowance_prices_every_token_at_the_dearer_rate() -> None:
    caps = Caps(max_tokens=10**9, max_usd=1.0, price=Price(1.0, 4.0))
    spend = Spend(caps)
    assert spend.allowance() == INVESTIGATION_TOKENS
    spend.add(Usage(output_tokens=240_000))  # $0.96 spent, $0.04 left: 10,000 tokens at $4
    assert spend.allowance() == 10_000


async def test_an_endpoint_reporting_no_tokens_stops_the_evaluation() -> None:
    out = await run(ScriptedAdapter(LOGS, propose(), tokens=0), runs=5)
    assert len(out.records) == 1
    assert "no token use" in out.stopped


async def test_logs_stay_out_of_the_prompt_above_the_ceiling() -> None:
    adapter = ScriptedAdapter(LOGS, DECLINE, DECLINE)
    out = await run_all([oom()], client(adapter, ceiling="internal"), 1, CAPS)
    assert out.records[0].conclusion == "abstained"
    assert not any("loading catalog" in m.content for m in adapter.requests[-1].messages)
