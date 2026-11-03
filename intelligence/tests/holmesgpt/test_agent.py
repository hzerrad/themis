"""The adapter asks HolmesGPT once per waiting incident, and suggests only
what passes Themis's checks."""

import json
from typing import Any

import httpx

from themis.v1 import common_pb2, proposal_pb2
from themis_intelligence.holmesgpt.agent import Agent
from themis_intelligence.holmesgpt.decide import SCHEMA
from themis_intelligence.holmesgpt.holmes import Holmes, HolmesError

TARGET = common_pb2.TargetRef(
    kind="Deployment", namespace="shop", name="checkout", uid="u1", group="apps"
)
HISTORY = json.dumps(
    {"revisions": [{"revision": "1"}, {"revision": "2", "current": True}]}
).encode()


class FakeThemis:
    def __init__(self, states: dict[str, str]) -> None:
        self.states = states
        self.submitted: list[proposal_pb2.SubmitProposalRequest] = []

    async def list_incidents(self) -> list[proposal_pb2.OpenIncident]:
        return [
            proposal_pb2.OpenIncident(id=i, target=TARGET, state=s) for i, s in self.states.items()
        ]

    async def get_incident(self, incident_id: str) -> proposal_pb2.GetIncidentResponse:
        return proposal_pb2.GetIncidentResponse(
            incident=proposal_pb2.OpenIncident(
                id=incident_id, target=TARGET, state=self.states[incident_id]
            ),
            evidence=[
                common_pb2.EvidenceItem(id="ev-history", template="rollout_history", data=HISTORY)
            ],
            predicates=[
                common_pb2.PredicateResult(
                    name="oom_killed_new_revision",
                    value=common_pb2.TRUTH_TRUE,
                    reason="r2 OOMKilled",
                )
            ],
        )

    async def submit(
        self, req: proposal_pb2.SubmitProposalRequest
    ) -> proposal_pb2.SubmitProposalResponse:
        self.submitted.append(req)
        return proposal_pb2.SubmitProposalResponse(status=proposal_pb2.PROPOSAL_STATUS_ACCEPTED)


def holmes_answering(analysis: Any, seen: list[dict[str, Any]], status: int = 200) -> Holmes:
    def handler(req: httpx.Request) -> httpx.Response:
        seen.append(
            {"path": req.url.path, "headers": dict(req.headers), "body": json.loads(req.content)}
        )
        return httpx.Response(status, json={"analysis": analysis})

    return Holmes(
        "http://holmes",
        "key",
        "gpt",
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )


GOOD = json.dumps(
    {
        "decision": "rollback",
        "namespace": "shop",
        "deployment": "checkout",
        "revision": 1,
        "rationale": "r2 runs out of memory",
        "evidence": ["ev-history"],
    }
)


async def test_asks_each_waiting_incident_once() -> None:
    themis = FakeThemis({"a": "awaiting_suggestion", "b": "investigating", "c": "inconclusive"})
    seen: list[dict[str, Any]] = []
    agent = Agent(themis, holmes_answering(GOOD, seen))
    assert await agent.run_once() == 2
    assert await agent.run_once() == 0
    assert [r.incident_id for r in themis.submitted] == ["a", "c"]
    req = themis.submitted[0]
    assert req.rollback_to_revision.revision == 1 and req.client_proposal_id == "holmesgpt-a"
    assert req.provenance.source == "holmesgpt" and req.target == TARGET
    asked = seen[0]
    assert asked["path"] == "/api/chat" and asked["headers"]["x-api-key"] == "key"
    assert asked["body"]["response_format"]["json_schema"]["schema"] == SCHEMA
    assert asked["body"]["model"] == "gpt" and "revision 2 (current)" in asked["body"]["ask"]
    assert "oom_killed_new_revision: true" in asked["body"]["ask"]


async def test_free_text_suggests_nothing() -> None:
    themis = FakeThemis({"a": "awaiting_suggestion"})
    await Agent(themis, holmes_answering("Roll back to revision 1.", [])).run_once()
    assert themis.submitted == []


async def test_holmes_failing_suggests_nothing() -> None:
    themis = FakeThemis({"a": "awaiting_suggestion"})
    await Agent(themis, holmes_answering(GOOD, [], status=500)).run_once()
    assert themis.submitted == []


async def test_holmes_errors_say_what_to_do() -> None:
    for status, says in [(401, "API key"), (404, "older than"), (500, "500")]:
        try:
            await holmes_answering(GOOD, [], status=status).ask("q", "i", SCHEMA)
        except HolmesError as e:
            assert says in str(e)
        else:
            raise AssertionError(f"{status} didn't raise")
