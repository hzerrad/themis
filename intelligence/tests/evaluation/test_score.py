from typing import Any

import pytest

from tests.evaluation.helpers import TARGET, oom
from themis_intelligence.evaluation.score import (
    HypothesisSeen,
    ProposalSeen,
    RunRecord,
    TargetSeen,
    metrics,
    predicate_disagreements,
    rate_of,
    score,
    wilson,
)


def record(
    conclusion: str = "proposed",
    revision: int = 1,
    target: dict[str, str] | None = None,
    cited: list[str] | None = None,
    predicates: list[str] | None = None,
    hypotheses: list[tuple[str, str]] | None = None,
    scenario: str = "rollout-oom",
    **fields: Any,
) -> RunRecord:
    proposal = None
    if conclusion == "proposed":
        proposal = ProposalSeen(
            target=TargetSeen(**(target or TARGET)),
            revision=revision,
            cited_evidence_ids=["ev-pod_states"] if cited is None else cited,
            cited_predicates=["oom_killed_new_revision"] if predicates is None else predicates,
        )
    return RunRecord(
        scenario=scenario,
        version=1,
        difficulty="easy",
        incident_class="rollout_regression",
        run=1,
        conclusion=conclusion,  # type: ignore[arg-type]
        proposal=proposal,
        hypotheses=[HypothesisSeen(root_cause_class=c, status=s) for c, s in hypotheses or []],
        shown=["ev-pod_states", "ev-rollout_history"],
        **fields,
    )


def test_right_rollback_is_correct_and_harmless() -> None:
    s = score(oom(), record())
    assert s.correct_action
    assert s.harmful == []
    assert s.citations_valid
    assert s.over_abstained is False
    assert s.forced_answer is None


@pytest.mark.parametrize("field", ["group", "kind", "namespace", "name", "uid"])
def test_any_other_target_is_harmful(field: str) -> None:
    s = score(oom(), record(target={**TARGET, field: "other"}))
    assert "another target" in s.harmful[0]
    assert not s.correct_action


def test_wrong_revision_is_harmful() -> None:
    s = score(oom(), record(revision=7))
    assert "revision 7" in s.harmful[0]
    assert not s.correct_action


def test_any_rollback_is_harmful_where_rollback_does_harm() -> None:
    b = oom(decision="abstain", acceptable_revisions=[], rollback_is_harmful=True)
    s = score(b, record())
    assert s.harmful
    assert s.forced_answer is True
    assert not s.correct_action


def test_declining_where_a_fix_is_right_is_over_abstention() -> None:
    s = score(oom(), record("abstained"))
    assert s.over_abstained is True
    assert not s.correct_action
    assert s.harmful == []
    assert s.citations_valid is None


def test_declining_where_truth_is_abstain_is_correct() -> None:
    s = score(oom(decision="abstain", acceptable_revisions=[]), record("abstained"))
    assert s.correct_action
    assert s.forced_answer is False
    assert s.over_abstained is None


def test_either_accepts_declining_and_a_harmless_rollback() -> None:
    b = oom(decision="either")
    assert score(b, record("abstained")).correct_action
    assert score(b, record()).correct_action
    assert not score(b, record(revision=7)).correct_action


def test_no_answer_is_never_correct() -> None:
    for decision in ("propose", "abstain", "either"):
        s = score(oom(decision=decision), record("failed"))
        assert not s.correct_action
        assert s.over_abstained in (False, None)


def test_running_out_of_steps_is_neither_right_nor_declining() -> None:
    for decision in ("propose", "abstain", "either"):
        s = score(oom(decision=decision), record("out_of_steps"))
        assert not s.correct_action
        assert s.over_abstained in (False, None)
        assert s.forced_answer in (False, None)


def test_root_cause_ranks_by_order_and_skips_refuted() -> None:
    right, wrong = ("memory_limit_regression", "supported"), ("bad_image", "supported")
    assert score(oom(), record(hypotheses=[right])).top1
    s = score(oom(), record(hypotheses=[wrong, right]))
    assert not s.top1
    assert s.top3
    s = score(oom(), record(hypotheses=[wrong, wrong, wrong, right]))
    assert not s.top3
    s = score(oom(), record(hypotheses=[("memory_limit_regression", "refuted"), wrong]))
    assert not s.top1
    assert not s.top3


def test_declining_is_the_root_cause_when_evidence_is_insufficient() -> None:
    b = oom(decision="abstain", root_causes=["insufficient_evidence"], acceptable_revisions=[])
    assert score(b, record("abstained")).top1
    assert not score(b, record("proposed", hypotheses=[("oom", "supported")])).top1


@pytest.mark.parametrize(
    ("cited", "predicates"),
    [
        (["ev-made-up", "ev-pod_states"], None),
        # Real, but never shown to the model.
        (["ev-container_logs_tail", "ev-pod_states"], None),
        (["ev-rollout_history"], None),  # leaves out the required evidence
        (None, ["gitops_managed"]),  # a check that's false
        (None, ["no_such_check"]),
    ],
)
def test_bad_citations(cited: list[str] | None, predicates: list[str] | None) -> None:
    assert score(oom(), record(cited=cited, predicates=predicates)).citations_valid is False


def test_wilson_matches_known_values() -> None:
    low, high = wilson(0.5, 10)
    assert low == pytest.approx(0.2366, abs=1e-3)
    assert high == pytest.approx(0.7634, abs=1e-3)
    assert wilson(0.0, 14)[0] == 0.0
    assert wilson(1.0, 14)[1] == 1.0
    assert wilson(0.0, 0) == (0.0, 1.0)


def test_interval_counts_scenarios_not_runs() -> None:
    # 100 repeats of one scenario are not 100 scenarios.
    one = rate_of(("a", True) for _ in range(100))
    many = rate_of((str(i), True) for i in range(100))
    assert one.rate == many.rate == 1.0
    assert one.low < 0.25
    assert many.low > 0.95


def test_metrics_leave_out_what_does_not_apply() -> None:
    b = oom()
    runs = [record(), record("abstained", scenario="other")]
    m = metrics([(r, score(b, r)) for r in runs])
    assert m.runs == 2
    assert (m.correct_action.hits, m.correct_action.total) == (1, 2)
    assert (m.over_abstained.hits, m.over_abstained.total) == (1, 2)
    assert m.forced_answer.total == 0
    assert m.forced_answer.rate is None
    assert m.citations_valid.total == 1


def test_call_counts_and_cost() -> None:
    b = oom()
    runs = [
        record(calls=4, clean_calls=3, repaired_calls=1, input_tokens=10, cost_usd=0.5),
        record(calls=1, clean_calls=1, output_tokens=5, cost_usd=0.25),
    ]
    m = metrics([(r, score(b, r)) for r in runs])
    assert (m.clean_calls.hits, m.clean_calls.total) == (4, 5)
    assert (m.repaired_calls.hits, m.repaired_calls.total) == (1, 5)
    assert (m.input_tokens, m.output_tokens) == (10, 5)
    assert m.cost_usd == 0.75


def test_predicate_disagreements() -> None:
    assert predicate_disagreements(oom()) == []
    b = oom(predicates={"oom_killed_new_revision": "false", "nope": "true"})
    assert predicate_disagreements(b) == [
        "rollout-oom: nope is missing, expected true",
        "rollout-oom: oom_killed_new_revision is true, expected false",
    ]
