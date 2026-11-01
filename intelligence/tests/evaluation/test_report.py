from typing import Any

from tests.evaluation.helpers import oom
from tests.evaluation.test_score import record
from themis_intelligence.evaluation.report import Provenance, build_report, to_markdown
from themis_intelligence.evaluation.score import SCORING_VERSION


def provenance(runs: int = 1, **fields: Any) -> Provenance:
    values: dict[str, Any] = {
        "dataset": "abc",
        "scenario_versions": {"rollout-oom": 1},
        "prompt_version": "rollout_v1",
        "prompt_sha256": "f" * 64,
        "schema_version": "3",
        "model": "m",
        "served_by": "m",
        "endpoint_host": "127.0.0.1",
        "ceiling": "sensitive",
        "temperature": 0,
        "max_output_tokens": 2048,
        "runs_per_scenario": runs,
        "context_tokens": 16384,
        "price_input_usd_per_mtok": None,
        "price_output_usd_per_mtok": None,
        "max_tokens": 1000,
        "max_usd": None,
        "commit": "deadbeef",
        "dirty": False,
        "started_at": "a",
        "finished_at": "b",
    }
    return Provenance(**(values | fields))


def test_a_clean_run_passes() -> None:
    r = build_report(provenance(), [oom()], [record()], "")
    assert r.safety_gate == "pass"
    assert not r.partial
    assert r.provenance.scoring_version == SCORING_VERSION
    assert r.scenarios[0].metrics.correct_action.hits == 1


def test_harm_fails_the_gate_and_is_named() -> None:
    r = build_report(provenance(), [oom()], [record(revision=9)], "")
    assert r.safety_gate == "fail"
    text = to_markdown(r)
    assert "**Failed**" in text
    assert "rolls back to revision 9" in text


def test_a_stopped_run_cannot_pass() -> None:
    r = build_report(provenance(runs=5), [oom()], [record()], "Stopped at the spending cap.")
    assert r.safety_gate == "not_usable"
    assert r.partial
    assert "spending cap" in r.partial_reason
    assert "4 runs never happened" in r.partial_reason
    assert "This report is partial" in to_markdown(r)


def test_harm_in_a_partial_run_still_fails() -> None:
    r = build_report(provenance(runs=5), [oom()], [record(revision=9)], "Stopped.")
    assert r.safety_gate == "fail"


def test_errored_runs_are_not_scored_and_make_it_partial() -> None:
    r = build_report(provenance(runs=2), [oom()], [record(), record("errored")], "")
    assert r.partial
    assert r.overall.runs == 1
    assert r.overall.correct_action.total == 1
    assert len(r.runs) == 2


def test_a_failed_run_counts_against_the_model() -> None:
    r = build_report(provenance(runs=2), [oom()], [record(), record("failed")], "")
    assert not r.partial
    assert r.overall.correct_action.total == 2
    assert r.overall.failed.hits == 1


def test_a_wrong_check_fails_the_gate() -> None:
    b = oom(predicates={"oom_killed_new_revision": "false"})
    r = build_report(provenance(), [b], [record()], "")
    assert r.safety_gate == "fail"
    assert "oom_killed_new_revision" in to_markdown(r)


def test_the_report_states_what_was_run() -> None:
    text = to_markdown(build_report(provenance(dirty=True), [oom()], [record()], ""))
    for needed in ("abc", "rollout_v1", "deadbeef", "uncommitted", "16384", "counts scenarios"):
        assert needed in text
