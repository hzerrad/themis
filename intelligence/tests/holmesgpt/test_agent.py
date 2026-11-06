"""The adapter asks HolmesGPT once per waiting incident, and suggests only
what passes Themis's checks."""

import asyncio
import contextlib
import json
from typing import Any

import grpc
import httpx
import pytest

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


# What HolmesGPT's /api/chat sends back for the tools it ran.
TOOL_CALLS: list[dict[str, Any]] = [
    {
        "tool_call_id": "1",
        "tool_name": "kubectl_describe",
        "description": "kubectl describe pod checkout-x -n shop",
        "result": {"status": "success", "data": "SECRET_FROM_A_LOG", "params": {}},
    },
    {
        "tool_call_id": "2",
        "tool_name": "fetch_logs",
        "description": "NA",
        "result": {"status": "error", "data": "x"},
    },
]


HISTORY_SO_FAR = [{"role": "system", "content": "..."}, {"role": "user", "content": "..."}]


def holmes_answering(analysis: Any, seen: list[dict[str, Any]], status: int = 200) -> Holmes:
    """A HolmesGPT that investigates with tools in the first turn and gives
    analysis as its verdict in the second."""

    def handler(req: httpx.Request) -> httpx.Response:
        body = json.loads(req.content)
        seen.append({"path": req.url.path, "headers": dict(req.headers), "body": body})
        reply: dict[str, Any]
        if "response_format" not in body:
            reply = {
                "analysis": "Revision 2 is OOMKilled at 64Mi.",
                "tool_calls": TOOL_CALLS,
                "conversation_history": HISTORY_SO_FAR,
            }
        else:
            reply = {"analysis": analysis, "tool_calls": []}
        return httpx.Response(status, json=reply)

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
    assert req.capability_name == "deployment.rollback-to-revision"
    assert (
        req.params.fields["revision"].number_value == 1 and req.client_proposal_id == "holmesgpt-a"
    )
    assert req.provenance.source == "holmesgpt" and req.target == TARGET
    # Two turns: free to run tools, then the verdict in the same conversation.
    investigate, conclude = seen[0], seen[1]
    assert investigate["path"] == "/api/chat" and investigate["headers"]["x-api-key"] == "key"
    assert "response_format" not in investigate["body"]
    assert investigate["body"]["model"] == "gpt"
    assert "revision 2 (current)" in investigate["body"]["ask"]
    assert "oom_killed_new_revision: true" in investigate["body"]["ask"]
    assert conclude["body"]["conversation_history"] == HISTORY_SO_FAR
    assert conclude["body"]["response_format"]["json_schema"]["schema"] == SCHEMA
    # What it ran and found go with the suggestion, for people; never what it read.
    assert req.notes == (
        "HolmesGPT ran 2 tools:\n"
        "- kubectl describe pod checkout-x -n shop (success)\n"
        "- fetch_logs (error)\n\n"
        "What HolmesGPT found:\nRevision 2 is OOMKilled at 64Mi."
    )
    assert "SECRET_FROM_A_LOG" not in req.notes


class Flaky(FakeThemis):
    """Answers or is unreachable as scripted, one poll at a time."""

    def __init__(self, *answers: bool) -> None:
        super().__init__({})
        self.answers = list(answers)
        self.done = asyncio.Event()

    async def list_incidents(self) -> list[proposal_pb2.OpenIncident]:
        if self.answers and not self.answers.pop(0):
            raise grpc.aio.AioRpcError(
                grpc.StatusCode.UNAVAILABLE, grpc.aio.Metadata(), grpc.aio.Metadata()
            )
        if not self.answers:
            self.done.set()
        return await super().list_incidents()


# Waiting for Themis at start is expected. An outage after that is logged when
# it starts and when it ends, not on every poll.
async def test_outages_are_logged_once(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level("INFO")
    themis = Flaky(False, False, True, False, False, False, True)
    agent = Agent(themis, holmes_answering(GOOD, []), poll_s=0)
    task = asyncio.create_task(agent.run())
    await asyncio.wait_for(themis.done.wait(), 1)
    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task
    said = [(r.levelname, r.getMessage()) for r in caplog.records if "Themis" in r.getMessage()]
    assert said == [
        ("INFO", "waiting for Themis to start"),
        ("WARNING", "Themis can't be reached"),
        ("INFO", "Themis reachable again"),
    ]


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
            await holmes_answering(GOOD, [], status=status).ask("q", "i", "c", SCHEMA)
        except HolmesError as e:
            assert says in str(e)
        else:
            raise AssertionError(f"{status} didn't raise")
