"""The loop: each incident waiting for a suggestion is put to HolmesGPT once."""

import asyncio
import logging
from collections.abc import Awaitable, Callable
from typing import Any, Protocol, cast

import grpc
from google.protobuf import struct_pb2

from themis.v1 import common_pb2, proposal_pb2, proposal_pb2_grpc
from themis_intelligence.holmesgpt.decide import NoSuggestion, decide, offered, verdict_schema
from themis_intelligence.holmesgpt.holmes import Holmes, HolmesError
from themis_intelligence.holmesgpt.prompt import INSTRUCTIONS, PROMPT_VERSION, conclude, describe

log = logging.getLogger(__name__)

CALL_TIMEOUT_S = 30.0
SOURCE = "holmesgpt"

# The states in which Themis takes a suggestion and nothing else is working on
# it. While Themis's own investigator runs, HolmesGPT isn't asked.
WAITING = {"awaiting_suggestion", "inconclusive"}


class Themis(Protocol):
    async def list_incidents(self) -> list[proposal_pb2.OpenIncident]: ...
    async def get_incident(self, incident_id: str) -> proposal_pb2.GetIncidentResponse: ...
    async def submit(
        self, req: proposal_pb2.SubmitProposalRequest
    ) -> proposal_pb2.SubmitProposalResponse: ...


class _Proposals(Protocol):
    def ListIncidents(
        self, request: proposal_pb2.ListIncidentsRequest, *, metadata: Any, timeout: float
    ) -> Awaitable[proposal_pb2.ListIncidentsResponse]: ...
    def GetIncident(
        self, request: proposal_pb2.GetIncidentRequest, *, metadata: Any, timeout: float
    ) -> Awaitable[proposal_pb2.GetIncidentResponse]: ...
    def SubmitProposal(
        self, request: proposal_pb2.SubmitProposalRequest, *, metadata: Any, timeout: float
    ) -> Awaitable[proposal_pb2.SubmitProposalResponse]: ...


class ThemisClient:
    """Themis's API for agents, with a Kubernetes service account token that
    is read on every call, since the kubelet rotates it."""

    def __init__(self, target: str, token: Callable[[], str]) -> None:
        self.channel = grpc.aio.insecure_channel(target)
        self.token = token
        self.stub = cast(_Proposals, proposal_pb2_grpc.ProposalServiceStub(self.channel))  # pyright: ignore[reportUnknownMemberType]

    def _md(self) -> tuple[tuple[str, str], ...]:
        return (("authorization", f"Bearer {self.token()}"),)

    async def list_incidents(self) -> list[proposal_pb2.OpenIncident]:
        resp = await self.stub.ListIncidents(
            proposal_pb2.ListIncidentsRequest(), metadata=self._md(), timeout=CALL_TIMEOUT_S
        )
        return list(resp.incidents)

    async def get_incident(self, incident_id: str) -> proposal_pb2.GetIncidentResponse:
        return await self.stub.GetIncident(
            proposal_pb2.GetIncidentRequest(incident_id=incident_id),
            metadata=self._md(),
            timeout=CALL_TIMEOUT_S,
        )

    async def submit(
        self, req: proposal_pb2.SubmitProposalRequest
    ) -> proposal_pb2.SubmitProposalResponse:
        return await self.stub.SubmitProposal(req, metadata=self._md(), timeout=CALL_TIMEOUT_S)

    async def close(self) -> None:
        await self.channel.close()


class Agent:
    def __init__(self, themis: Themis, holmes: Holmes, poll_s: float = 15.0) -> None:
        self.themis = themis
        self.holmes = holmes
        self.poll_s = poll_s
        # Incidents already put to HolmesGPT. Each costs the team's tokens and
        # minutes, so each is asked once; a restart may ask again, and Themis
        # takes the same suggestion only once.
        self.asked: set[str] = set()

    async def run(self) -> None:
        # Themis may start after the adapter: until it first answers, waiting
        # for it is expected, not a problem. An outage after that is logged
        # when it starts, when its cause changes and when it ends, not on
        # every poll.
        connected = False
        outage: grpc.StatusCode | None = None
        while True:
            try:
                await self.run_once()
            except grpc.RpcError as e:
                code = cast(grpc.Call, e).code()
                if code != outage:
                    error = code.name if code else str(e)
                    if connected:
                        log.warning("Themis can't be reached", extra={"error": error})
                    else:
                        log.info("waiting for Themis to start", extra={"error": error})
                    outage = code
            else:
                if connected and outage is not None:
                    log.info("Themis reachable again")
                connected, outage = True, None
            await asyncio.sleep(self.poll_s)

    async def run_once(self) -> int:
        """Puts each new waiting incident to HolmesGPT; returns how many."""
        n = 0
        for inc in await self.themis.list_incidents():
            if inc.state not in WAITING or inc.id in self.asked:
                continue
            self.asked.add(inc.id)
            await self.handle(inc.id)
            n += 1
        return n

    async def handle(self, incident_id: str) -> None:
        incident = await self.themis.get_incident(incident_id)
        target = incident.incident.target
        # The fixes Themis lets this source suggest for the incident: those its
        # profiles offer that the source's own list names.
        fixes = offered(incident.capabilities)
        if not fixes:
            log.info(
                "HolmesGPT not asked",
                extra={"incident": incident_id, "reason": "the incident offers it no fix"},
            )
            return
        evidence_ids = [e.id for e in incident.evidence]
        log.info(
            "asking HolmesGPT",
            extra={"incident": incident_id, "target": f"{target.namespace}/{target.name}"},
        )
        try:
            answer = await self.holmes.ask(
                describe(incident, fixes),
                INSTRUCTIONS,
                conclude(fixes),
                verdict_schema(fixes, evidence_ids),
            )
        except HolmesError as e:
            log.warning(
                "HolmesGPT couldn't answer", extra={"incident": incident_id, "error": str(e)}
            )
            return
        log.info(
            "HolmesGPT answered", extra={"incident": incident_id, "tools_run": answer.tools_run}
        )
        outcome = decide(answer.analysis, target, fixes, set(evidence_ids))
        if isinstance(outcome, NoSuggestion):
            log.info("no suggestion", extra={"incident": incident_id, "reason": outcome.reason})
            return
        params = struct_pb2.Struct()
        params.update(outcome.params)
        resp = await self.themis.submit(
            proposal_pb2.SubmitProposalRequest(
                incident_id=incident_id,
                client_proposal_id=f"{SOURCE}-{incident_id}",
                target=target,
                capability_name=outcome.fix,
                params=params,
                rationale=outcome.rationale,
                expected_effect=outcome.expected_effect,
                cited_evidence_ids=list(outcome.cited),
                notes=answer.notes,
                provenance=common_pb2.Provenance(
                    source=SOURCE,
                    model=self.holmes.model or "",
                    prompt_version=PROMPT_VERSION,
                    route=SOURCE,
                ),
            )
        )
        if resp.status == proposal_pb2.PROPOSAL_STATUS_ACCEPTED:
            log.info("suggested a fix", extra={"incident": incident_id, "fix": outcome.fix})
        else:
            log.warning(
                "Themis didn't accept the suggestion",
                extra={"incident": incident_id, "fix": outcome.fix, "reason": resp.message},
            )
