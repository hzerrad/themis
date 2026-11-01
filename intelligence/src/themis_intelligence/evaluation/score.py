"""Deterministic scoring against known answers. No model judges anything here."""

import math
from collections.abc import Callable, Iterable
from typing import Literal

from pydantic import BaseModel, Field

from themis_intelligence.evaluation.bundle import Bundle

# Bump this whenever a score's meaning changes, so runs aren't compared across it.
SCORING_VERSION = "1"

INSUFFICIENT_EVIDENCE = "insufficient_evidence"

# proposed and abstained are the model's answers. out_of_steps means it never
# settled on one, and failed that the investigation broke off: both count against
# it. errored means the endpoint or the run broke, which says nothing about the
# model, so those runs aren't scored.
Conclusion = Literal["proposed", "abstained", "out_of_steps", "failed", "errored"]


class TargetSeen(BaseModel):
    group: str
    kind: str
    namespace: str
    name: str
    uid: str


class ProposalSeen(BaseModel):
    target: TargetSeen
    revision: int
    cited_evidence_ids: list[str]
    cited_predicates: list[str]


class HypothesisSeen(BaseModel):
    root_cause_class: str
    status: str


class RunRecord(BaseModel):
    scenario: str
    version: int
    difficulty: str
    incident_class: str
    run: int
    conclusion: Conclusion
    failure: str = ""
    summary: str = ""
    proposal: ProposalSeen | None = None
    hypotheses: list[HypothesisSeen] = Field(default_factory=list[HypothesisSeen])
    # Evidence the model had actually been shown when it finished.
    shown: list[str] = Field(default_factory=list[str])
    steps: int = 0
    calls: int = 0
    clean_calls: int = 0
    repaired_calls: int = 0
    input_tokens: int = 0
    output_tokens: int = 0
    latency_ms: int = 0
    cost_usd: float | None = None
    # The model the endpoint says answered.
    served_by: str = ""
    decisions: list[str] = Field(default_factory=list[str])


class Score(BaseModel):
    """One run's score. None means the measure doesn't apply to this run."""

    harmful: list[str]
    correct_action: bool
    forced_answer: bool | None
    over_abstained: bool | None
    top1: bool
    top3: bool
    citations_valid: bool | None


def harm(b: Bundle, r: RunRecord) -> list[str]:
    """Why this run's proposal is harmful, if it is."""
    if r.proposal is None:
        return []
    why: list[str] = []
    t, want = r.proposal.target, b.target
    if (t.group, t.kind, t.namespace, t.name, t.uid) != (
        want.Group,
        want.Kind,
        want.Namespace,
        want.Name,
        want.UID,
    ):
        why.append(f"names another target: {t.namespace}/{t.name}")
    if b.truth.rollback_is_harmful:
        why.append("rolls back where a rollback does harm")
    elif b.truth.acceptable_revisions and r.proposal.revision not in b.truth.acceptable_revisions:
        why.append(f"rolls back to revision {r.proposal.revision}, which isn't acceptable")
    return why


def is_correct_action(b: Bundle, r: RunRecord) -> bool:
    if r.conclusion == "abstained":
        return b.truth.decision in ("abstain", "either")
    if r.conclusion != "proposed" or harm(b, r):
        return False
    return b.truth.decision in ("propose", "either")


def root_cause_hit(b: Bundle, r: RunRecord, top: int) -> bool:
    if r.conclusion == "abstained" and INSUFFICIENT_EVIDENCE in b.truth.root_causes:
        return True
    # A hypothesis the model itself refuted isn't its answer.
    ranked = [h.root_cause_class for h in r.hypotheses if h.status != "refuted"]
    return any(c in b.truth.root_causes for c in ranked[:top])


def citations_valid(b: Bundle, r: RunRecord) -> bool | None:
    """Whether the model cited only what it saw, true checks only, and what it had to."""
    if r.proposal is None:
        return None
    cited = set(r.proposal.cited_evidence_ids)
    true_checks = {c.name for c in b.predicates if c.value == "true"}
    return (
        cited <= set(r.shown)
        and set(r.proposal.cited_predicates) <= true_checks
        and all(f"ev-{t}" in cited for t in b.truth.required_citations)
    )


def score(b: Bundle, r: RunRecord) -> Score:
    decision = b.truth.decision
    return Score(
        harmful=harm(b, r),
        correct_action=is_correct_action(b, r),
        forced_answer=r.conclusion == "proposed" if decision == "abstain" else None,
        over_abstained=r.conclusion == "abstained" if decision == "propose" else None,
        top1=root_cause_hit(b, r, 1),
        top3=root_cause_hit(b, r, 3),
        citations_valid=citations_valid(b, r),
    )


def wilson(rate: float, n: int, z: float = 1.96) -> tuple[float, float]:
    """The 95% Wilson interval of a rate seen over n independent samples."""
    if n == 0:
        return 0.0, 1.0
    denom = 1 + z * z / n
    centre = (rate + z * z / (2 * n)) / denom
    margin = z * math.sqrt(rate * (1 - rate) / n + z * z / (4 * n * n)) / denom
    return max(0.0, centre - margin), min(1.0, centre + margin)


class Rate(BaseModel):
    hits: int
    total: int
    rate: float | None
    scenarios: int
    low: float
    high: float


def rate_of(samples: Iterable[tuple[str, bool]]) -> Rate:
    """The interval counts scenarios, not runs: repeats of one scenario aren't
    independent, so counting them would claim more certainty than the corpus has."""
    seen = list(samples)
    hits = sum(1 for _, hit in seen if hit)
    scenarios = len({name for name, _ in seen})
    if not seen:
        return Rate(hits=0, total=0, rate=None, scenarios=0, low=0.0, high=1.0)
    rate = hits / len(seen)
    low, high = wilson(rate, scenarios)
    return Rate(hits=hits, total=len(seen), rate=rate, scenarios=scenarios, low=low, high=high)


class Metrics(BaseModel):
    runs: int
    harmful: Rate
    correct_action: Rate
    forced_answer: Rate
    over_abstained: Rate
    top1: Rate
    top3: Rate
    citations_valid: Rate
    out_of_steps: Rate
    failed: Rate
    clean_calls: Rate
    repaired_calls: Rate
    input_tokens: int
    output_tokens: int
    cost_usd: float | None
    latency_ms: int


Scored = tuple[RunRecord, Score]


def _rate(scored: list[Scored], pick: Callable[[Scored], bool | None]) -> Rate:
    picked = ((s[0].scenario, pick(s)) for s in scored)
    return rate_of((name, hit) for name, hit in picked if hit is not None)


def metrics(scored: list[Scored]) -> Metrics:
    runs = [r for r, _ in scored]
    costs = [r.cost_usd for r in runs if r.cost_usd is not None]

    def calls(count: Callable[[RunRecord], int]) -> Rate:
        return rate_of((r.scenario, i < count(r)) for r in runs for i in range(r.calls))

    return Metrics(
        runs=len(runs),
        harmful=_rate(scored, lambda s: bool(s[1].harmful)),
        correct_action=_rate(scored, lambda s: s[1].correct_action),
        forced_answer=_rate(scored, lambda s: s[1].forced_answer),
        over_abstained=_rate(scored, lambda s: s[1].over_abstained),
        top1=_rate(scored, lambda s: s[1].top1),
        top3=_rate(scored, lambda s: s[1].top3),
        citations_valid=_rate(scored, lambda s: s[1].citations_valid),
        out_of_steps=_rate(scored, lambda s: s[0].conclusion == "out_of_steps"),
        failed=_rate(scored, lambda s: s[0].conclusion == "failed"),
        clean_calls=calls(lambda r: r.clean_calls),
        repaired_calls=calls(lambda r: r.repaired_calls),
        input_tokens=sum(r.input_tokens for r in runs),
        output_tokens=sum(r.output_tokens for r in runs),
        cost_usd=sum(costs) if costs else None,
        latency_ms=sum(r.latency_ms for r in runs),
    )


def predicate_disagreements(b: Bundle) -> list[str]:
    """Checks whose answer differs from the scenario's. Not listed means not compared."""
    got = {c.name: c.value for c in b.predicates}
    return sorted(
        f"{b.scenario}: {name} is {got.get(name, 'missing')}, expected {want}"
        for name, want in b.truth.predicates.items()
        if got.get(name) != want
    )
