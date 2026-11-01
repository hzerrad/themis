from typing import Any

from tests.investigation.fakes import FakeCore, ScriptedModel, job
from themis.v1 import investigation_pb2
from themis_intelligence.investigation.loop import investigate

LOGS: dict[str, Any] = {"kind": "request_evidence", "template": "container_logs_tail", "params": {}}
GIVE_UP: dict[str, Any] = {"kind": "conclude_inconclusive"}


def causes(gaps: list[investigation_pb2.EvidenceGap]) -> set[tuple[str, int]]:
    return {(g.template, g.cause) for g in gaps}


# Logs the model may not see stay out of the prompt, and the investigation
# goes on without them instead of failing as if the model were down.
async def test_sensitive_evidence_withheld_not_failed() -> None:
    core = FakeCore(logs="canary-log-line")
    model = ScriptedModel(LOGS, GIVE_UP, ceiling="internal")
    gaps: list[investigation_pb2.EvidenceGap] = []

    result = await investigate(job(), core, model, max_steps=6, gaps=gaps)

    assert result.conclusion == "inconclusive"
    for req in model.requests:
        assert req.data_classification == "internal"
        assert all("canary-log-line" not in m.content for m in req.messages)
    assert "may not see sensitive data" in model.requests[1].messages[-1].content
    assert causes(gaps) == {
        ("container_logs_tail", investigation_pb2.EVIDENCE_GAP_CAUSE_DATA_CEILING)
    }


# An endpoint cleared for sensitive data sees the logs, and nothing is missing.
async def test_cleared_endpoint_sees_the_logs() -> None:
    core = FakeCore(logs="canary-log-line")
    model = ScriptedModel(LOGS, GIVE_UP)
    gaps: list[investigation_pb2.EvidenceGap] = []
    await investigate(job(), core, model, max_steps=6, gaps=gaps)
    assert "canary-log-line" in model.requests[1].messages[-1].content
    assert gaps == []


# Before giving up, the model is asked once to read what it hasn't.
async def test_abstention_tries_allowed_templates_first() -> None:
    model = ScriptedModel(GIVE_UP, GIVE_UP)
    result = await investigate(job(), FakeCore(), model, max_steps=6)
    assert result.conclusion == "inconclusive"
    assert len(model.requests) == 2
    assert "container_logs_tail" in model.requests[1].messages[-1].content


# Once everything allowed was read, giving up is accepted at once.
async def test_abstention_after_reading_everything_is_immediate() -> None:
    model = ScriptedModel(LOGS, GIVE_UP)
    result = await investigate(job(), FakeCore(), model, max_steps=6)
    assert result.conclusion == "inconclusive"
    assert len(model.requests) == 2


# Evidence the profile doesn't allow is reported, so the operator can learn
# that it would have helped.
async def test_template_outside_the_profile_is_reported() -> None:
    model = ScriptedModel(
        {"kind": "request_evidence", "template": "target_events", "params": {}}, LOGS, GIVE_UP
    )
    gaps: list[investigation_pb2.EvidenceGap] = []
    await investigate(job(), FakeCore(), model, max_steps=6, gaps=gaps)
    assert causes(gaps) == {("target_events", investigation_pb2.EVIDENCE_GAP_CAUSE_NOT_IN_PROFILE)}
