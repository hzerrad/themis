"""Runs every scenario N times through the real investigation loop and model client."""

from collections.abc import Callable
from dataclasses import dataclass, field

from pydantic import ValidationError

from themis.v1 import investigation_pb2
from themis_intelligence.evaluation.bundle import Bundle, job_of
from themis_intelligence.evaluation.replay import ModelUse, RecordingModel, ReplayCore
from themis_intelligence.evaluation.score import (
    Conclusion,
    HypothesisSeen,
    ProposalSeen,
    RunRecord,
    TargetSeen,
)
from themis_intelligence.investigation.loop import InvestigationError, Model, investigate
from themis_intelligence.platform.client import Price
from themis_intelligence.platform.contract import Usage

# The same limit an investigation has in production by default.
INVESTIGATION_TOKENS = 200_000
_STATUSES = {
    investigation_pb2.HYPOTHESIS_STATUS_SUPPORTED: "supported",
    investigation_pb2.HYPOTHESIS_STATUS_REFUTED: "refuted",
}

# In a row: past this the endpoint is down, and more runs would only add noise.
MAX_ERRORS_IN_A_ROW = 3


@dataclass(frozen=True)
class Caps:
    """Limits on one whole evaluation. A call that would pass them, by estimate, isn't made."""

    max_tokens: int
    max_usd: float | None = None
    price: Price | None = None


@dataclass
class Spend:
    caps: Caps
    tokens: int = 0
    usd: float = 0.0

    def add(self, usage: Usage) -> None:
        self.tokens += usage.input_tokens + usage.output_tokens
        if self.caps.price is not None:
            self.usd += self.caps.price.cost(usage)

    def allowance(self) -> int:
        """Tokens the next investigation may use, pricing all of them at the dearer rate."""
        left = min(INVESTIGATION_TOKENS, self.caps.max_tokens - self.tokens)
        price = self.caps.price
        if self.caps.max_usd is not None and price is not None:
            dearest = max(price.input_usd_per_mtok, price.output_usd_per_mtok)
            if dearest > 0:
                left = min(left, int((self.caps.max_usd - self.usd) / dearest * 1_000_000))
        return max(0, left)


@dataclass
class Outcome:
    records: list[RunRecord] = field(default_factory=list[RunRecord])
    # Why the evaluation stopped early, if it did.
    stopped: str = ""


def _record(
    b: Bundle,
    run: int,
    core: ReplayCore,
    use: ModelUse,
    price: Price | None,
    conclusion: Conclusion,
    failure: str = "",
    summary: str = "",
    hypotheses: list[HypothesisSeen] | None = None,
    steps: int = 0,
) -> RunRecord:
    proposal = None
    if core.proposals:
        p = core.proposals[0]
        proposal = ProposalSeen(
            target=TargetSeen(
                group=p.target.group,
                kind=p.target.kind,
                namespace=p.target.namespace,
                name=p.target.name,
                uid=p.target.uid,
            ),
            revision=p.rollback_to_revision.revision,
            cited_evidence_ids=list(p.cited_evidence_ids),
            cited_predicates=list(p.cited_predicates),
        )
    return RunRecord(
        scenario=b.scenario,
        version=b.version,
        difficulty=b.difficulty,
        incident_class=b.incident_class,
        run=run,
        conclusion=conclusion,
        failure=failure,
        summary=summary,
        proposal=proposal,
        hypotheses=hypotheses or [],
        shown=sorted(core.shown),
        steps=steps,
        calls=use.calls,
        clean_calls=use.clean,
        repaired_calls=use.repaired,
        input_tokens=use.usage.input_tokens,
        output_tokens=use.usage.output_tokens,
        latency_ms=use.latency_ms,
        cost_usd=price.cost(use.usage) if price is not None else None,
        served_by=use.model,
        decisions=use.decisions,
    )


def _conclusion(reached: str, use: ModelUse) -> Conclusion:
    if reached == "proposed":
        return "proposed"
    # The core hears "inconclusive" either way, but only one of these is a decision.
    declined = use.decisions[-1:] == ["conclude_inconclusive"]
    return "abstained" if declined else "out_of_steps"


async def run_one(b: Bundle, run: int, model: Model, price: Price | None) -> tuple[RunRecord, bool]:
    """One investigation, and whether it ran out of tokens."""
    core = ReplayCore(b)
    recording = RecordingModel(model)
    try:
        result = await investigate(job_of(b), core, recording, max_steps=b.max_steps)
    except InvestigationError as e:
        conclusion: Conclusion = "errored" if e.reason == "model_unavailable" else "failed"
        return (
            _record(b, run, core, recording.use, price, conclusion, failure=str(e)),
            e.reason == "budget_exhausted",
        )
    except (ValidationError, IndexError) as e:
        # A reply the adapter can't read at all says something broke, not what the model thinks.
        failure = f"unreadable reply: {type(e).__name__}"
        return _record(b, run, core, recording.use, price, "errored", failure=failure), False
    return (
        _record(
            b,
            run,
            core,
            recording.use,
            price,
            _conclusion(result.conclusion, recording.use),
            summary=result.summary,
            hypotheses=[
                HypothesisSeen(
                    root_cause_class=h.root_cause_class,
                    status=_STATUSES.get(h.status, "open"),
                )
                for h in result.hypotheses
            ],
            steps=result.usage.steps_used,
        ),
        False,
    )


async def run_all(
    bundles: list[Bundle],
    new_model: Callable[[int], Model],
    runs: int,
    caps: Caps,
    progress: Callable[[RunRecord], None] = lambda _: None,
) -> Outcome:
    """new_model gets the tokens an investigation may use."""
    out = Outcome()
    spend = Spend(caps)
    errors = 0
    for b in bundles:
        for run in range(1, runs + 1):
            allowance = spend.allowance()
            record, ran_out = await run_one(b, run, new_model(allowance), caps.price)
            spend.add(Usage(input_tokens=record.input_tokens, output_tokens=record.output_tokens))
            # Out of tokens because of the evaluation's cap says nothing about
            # the model, so this run is dropped. The per-investigation limit is
            # part of what's measured, so running into it counts as a failure.
            if ran_out and allowance < INVESTIGATION_TOKENS:
                out.stopped = (
                    f"Stopped at the spending cap, after {spend.tokens} tokens"
                    f" and ${spend.usd:.4f}."
                )
                return out
            out.records.append(record)
            progress(record)
            silent = not record.input_tokens + record.output_tokens
            if record.calls and silent and record.conclusion != "errored":
                out.stopped = "The endpoint reports no token use, so the cap can't be enforced."
                return out
            errors = errors + 1 if record.conclusion == "errored" else 0
            if errors >= MAX_ERRORS_IN_A_ROW:
                out.stopped = f"Stopped after {errors} errors in a row: {record.failure}"
                return out
    return out
