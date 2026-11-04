from pathlib import Path

import pytest

from tests.evaluation.helpers import oom
from themis.v1 import common_pb2, evidence_pb2
from themis_intelligence.evaluation.bundle import Bundle, job_of, load_bundles
from themis_intelligence.evaluation.replay import BudgetSpentError, ReplayCore
from themis_intelligence.investigation.loop import incident_prompt, system_prompt
from themis_intelligence.investigation.schemas import ROOT_CAUSES


def test_job_matches_the_bundle() -> None:
    job = job_of(oom())
    assert job.target.uid == "uid-deployment-checkout"
    assert job.initial_evidence[0].template == "rollout_history"
    assert {t.name for t in job.allowed_templates} >= {"container_logs_tail"}
    assert all(t.description for t in job.allowed_templates)
    logs = next(t for t in job.allowed_templates if t.name == "container_logs_tail")
    assert list(logs.params) == ["pod", "container", "previous"]
    check = next(p for p in job.predicates if p.name == "oom_killed_new_revision")
    assert check.value == common_pb2.TRUTH_TRUE
    assert job.initial_evidence[0].classification == common_pb2.DATA_CLASSIFICATION_INTERNAL


ALL = load_bundles(Path(__file__).parents[3] / "scenarios")


def test_the_corpus_covers_the_spec() -> None:
    assert len(ALL) >= 14
    # Spec 07 asks for at least two where the right answer is that evidence is short.
    short = [b for b in ALL if "insufficient_evidence" in b.truth.root_causes]
    assert len(short) >= 2
    assert {b.difficulty for b in ALL} == {"easy", "medium", "hard"}


@pytest.mark.parametrize("b", ALL, ids=lambda b: b.scenario)
def test_prompt_gives_nothing_away(b: Bundle) -> None:
    job = job_of(b)
    prompt = system_prompt(job, b.max_steps) + incident_prompt(job)
    assert b.scenario not in prompt
    for marker in b.truth.must_not_say:
        assert marker not in prompt
    # Every root cause is listed once, the answer no more than the rest.
    for answer in b.truth.root_causes:
        assert prompt.count(answer) == 1


def test_load_refuses_an_unknown_case_or_pack() -> None:
    root = Path(__file__).parents[3] / "scenarios"
    picked = load_bundles(root, ["rollout-oom"], ["rollout-regression"])
    assert [b.scenario for b in picked] == ["rollout-oom"]
    with pytest.raises(ValueError, match="no-such"):
        load_bundles(root, ["no-such"])
    with pytest.raises(ValueError, match="no-pack"):
        load_bundles(root, packs=["no-pack"])


async def test_replay_serves_and_counts_evidence() -> None:
    core = ReplayCore(oom())
    assert "ev-container_logs_tail" not in core.shown
    req = evidence_pb2.CollectEvidenceRequest(template="container_logs_tail")
    resp = await core.collect_evidence(req)
    assert resp.items[0].id == "ev-container_logs_tail"
    assert resp.items[0].classification == common_pb2.DATA_CLASSIFICATION_SENSITIVE
    assert "ev-container_logs_tail" in core.shown
    for _ in range(core.bundle.max_evidence_requests - 1):
        await core.collect_evidence(req)
    with pytest.raises(BudgetSpentError):
        await core.collect_evidence(req)


@pytest.mark.parametrize("b", ALL, ids=lambda b: b.scenario)
def test_every_answer_is_on_the_list(b: Bundle) -> None:
    assert set(b.truth.root_causes) <= set(ROOT_CAUSES)
