from pathlib import Path

import pytest

from tests.evaluation.helpers import oom
from themis.v1 import common_pb2, evidence_pb2
from themis_intelligence.evaluation.bundle import job_of, load_bundles
from themis_intelligence.evaluation.replay import BudgetSpentError, ReplayCore
from themis_intelligence.investigation.loop import incident_prompt, system_prompt


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


def test_prompt_gives_nothing_away() -> None:
    b = oom()
    job = job_of(b)
    prompt = system_prompt(job, b.max_steps) + incident_prompt(job)
    assert b.scenario not in prompt
    for answer in b.truth.root_causes:
        assert answer not in prompt


def test_load_refuses_an_unknown_scenario() -> None:
    root = Path(__file__).parents[3] / "eval" / "scenarios"
    assert [b.scenario for b in load_bundles(root, ["rollout-oom"])] == ["rollout-oom"]
    with pytest.raises(ValueError, match="no-such"):
        load_bundles(root, ["no-such"])


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
