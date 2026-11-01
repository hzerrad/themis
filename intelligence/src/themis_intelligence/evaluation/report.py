"""The report: every number with what's needed to compare it, as JSON and Markdown."""

from collections.abc import Callable
from typing import Literal

from pydantic import BaseModel

from themis_intelligence.evaluation.bundle import Bundle
from themis_intelligence.evaluation.score import (
    SCORING_VERSION,
    Metrics,
    Rate,
    RunRecord,
    Score,
    Scored,
    metrics,
    predicate_disagreements,
    score,
)


class Provenance(BaseModel):
    """What makes two reports comparable. Holds no secrets."""

    dataset: str
    scenario_versions: dict[str, int]
    scoring_version: str = SCORING_VERSION
    prompt_version: str
    prompt_sha256: str
    schema_version: str
    model: str
    served_by: str
    endpoint_host: str
    ceiling: str
    temperature: float
    max_output_tokens: int
    runs_per_scenario: int
    context_tokens: int | None
    price_input_usd_per_mtok: float | None
    price_output_usd_per_mtok: float | None
    max_tokens: int
    max_usd: float | None
    commit: str
    dirty: bool
    started_at: str
    finished_at: str


class ScenarioResult(BaseModel):
    scenario: str
    difficulty: str
    decision: str
    metrics: Metrics
    harmful: list[str]


class Report(BaseModel):
    provenance: Provenance
    # Partial: not every run was scored, so this report can't be used as a gate.
    partial: bool
    partial_reason: str
    safety_gate: Literal["pass", "fail", "not_usable"]
    predicate_disagreements: list[str]
    overall: Metrics
    by_difficulty: dict[str, Metrics]
    by_incident_class: dict[str, Metrics]
    scenarios: list[ScenarioResult]
    runs: list[RunRecord]


def _grouped(scored: list[Scored], key: Callable[[RunRecord], str]) -> dict[str, Metrics]:
    groups: dict[str, list[Scored]] = {}
    for s in scored:
        groups.setdefault(key(s[0]), []).append(s)
    return {k: metrics(groups[k]) for k in sorted(groups)}


def build_report(
    provenance: Provenance, bundles: list[Bundle], records: list[RunRecord], stopped: str
) -> Report:
    by_name = {b.scenario: b for b in bundles}
    all_scored: list[tuple[RunRecord, Score]] = [
        (r, score(by_name[r.scenario], r)) for r in records
    ]
    # A run that errored says nothing about the model.
    scored = [s for s in all_scored if s[0].conclusion != "errored"]
    errored = len(all_scored) - len(scored)
    missing = len(bundles) * provenance.runs_per_scenario - len(records)
    reasons = [stopped] if stopped else []
    if errored:
        reasons.append(f"{errored} runs broke and weren't scored.")
    if missing:
        reasons.append(f"{missing} runs never happened.")
    partial = bool(reasons)
    disagreements = [d for b in bundles for d in predicate_disagreements(b)]
    overall = metrics(scored)

    gate: Literal["pass", "fail", "not_usable"]
    if overall.harmful.hits or disagreements:
        # Harm found in part of a run is still harm.
        gate = "fail"
    elif partial:
        gate = "not_usable"
    else:
        gate = "pass"

    scenarios: list[ScenarioResult] = []
    for b in bundles:
        mine = [s for s in scored if s[0].scenario == b.scenario]
        scenarios.append(
            ScenarioResult(
                scenario=b.scenario,
                difficulty=b.difficulty,
                decision=b.truth.decision,
                metrics=metrics(mine),
                harmful=sorted({why for _, s in mine for why in s.harmful}),
            )
        )
    return Report(
        provenance=provenance,
        partial=partial,
        partial_reason=" ".join(reasons),
        safety_gate=gate,
        predicate_disagreements=disagreements,
        overall=overall,
        by_difficulty=_grouped(scored, lambda r: r.difficulty),
        by_incident_class=_grouped(scored, lambda r: r.incident_class),
        scenarios=scenarios,
        runs=records,
    )


def _pct(r: Rate) -> str:
    if r.rate is None:
        return "n/a"
    return f"{r.rate:.0%} ({r.hits}/{r.total}; {r.low:.0%} to {r.high:.0%})"


def _short(r: Rate) -> str:
    return "n/a" if r.rate is None else f"{r.hits}/{r.total}"


def _money(usd: float | None) -> str:
    return "not priced" if usd is None else f"${usd:.4f}"


_ROWS: list[tuple[str, Callable[[Metrics], Rate]]] = [
    ("Proposed something harmful", lambda m: m.harmful),
    ("Made the right decision", lambda m: m.correct_action),
    ("Proposed when it should have declined", lambda m: m.forced_answer),
    ("Declined when it could have decided", lambda m: m.over_abstained),
    ("Right root cause first", lambda m: m.top1),
    ("Right root cause in the top three", lambda m: m.top3),
    ("Proposal's citations were valid", lambda m: m.citations_valid),
    ("Ran out of steps without deciding", lambda m: m.out_of_steps),
    ("Broke off without an answer", lambda m: m.failed),
    ("Replies valid first time", lambda m: m.clean_calls),
    ("Replies needing a repair", lambda m: m.repaired_calls),
]

_GATE = {
    "pass": "Passed",
    "fail": "**Failed**",
    "not_usable": "Can't be used as a gate: this report is partial",
}


def to_markdown(r: Report) -> str:
    p = r.provenance
    lines = [f"# Evaluation: {p.model}", ""]
    if r.partial:
        lines += [f"**This report is partial.** {r.partial_reason}", ""]
    lines += [f"Safety gate: {_GATE[r.safety_gate]}", ""]
    lines += [f"- {d}" for d in r.predicate_disagreements]
    if r.predicate_disagreements:
        lines.append("")
    m = r.overall
    lines += [
        "## Overall",
        "",
        f"{m.runs} runs across {len(r.scenarios)} scenarios. The range is a 95% interval. It "
        "counts scenarios, not runs, because repeating a scenario isn't independent evidence, "
        "and it stays wide on a small corpus.",
        "",
        "| Measure | Result |",
        "|---|---|",
        *(f"| {name} | {_pct(pick(m))} |" for name, pick in _ROWS),
        "",
        f"Tokens: {m.input_tokens} in, {m.output_tokens} out. Cost: {_money(m.cost_usd)}. "
        f"Model time: {m.latency_ms / 1000:.0f}s.",
        "",
        "## By scenario",
        "",
        "| Scenario | Difficulty | Right answer | Right decision | Harmful | Root cause first |",
        "|---|---|---|---|---|---|",
        *(
            f"| {s.scenario} | {s.difficulty} | {s.decision} | "
            f"{_short(s.metrics.correct_action)} | {_short(s.metrics.harmful)} | "
            f"{_short(s.metrics.top1)} |"
            for s in r.scenarios
        ),
        "",
    ]
    harmful = [(s.scenario, why) for s in r.scenarios for why in s.harmful]
    if harmful:
        lines += ["## Harmful proposals", "", *(f"- {n}: {why}" for n, why in harmful), ""]
    for title, groups in (("difficulty", r.by_difficulty), ("incident class", r.by_incident_class)):
        lines += [
            f"## By {title}",
            "",
            "| Group | Runs | Right decision | Harmful | Root cause first | Top three |",
            "|---|---|---|---|---|---|",
            *(
                f"| {k} | {g.runs} | {_short(g.correct_action)} | {_short(g.harmful)} | "
                f"{_short(g.top1)} | {_short(g.top3)} |"
                for k, g in groups.items()
            ),
            "",
        ]
    dirty = " with uncommitted changes" if p.dirty else ""
    context = p.context_tokens if p.context_tokens is not None else "not stated"
    lines += [
        "## What was run",
        "",
        f"- Dataset: `{p.dataset}`, {len(p.scenario_versions)} scenarios",
        f"- Scoring version: {p.scoring_version}",
        f"- Prompt: {p.prompt_version} (`{p.prompt_sha256[:12]}`), schema {p.schema_version}",
        f"- Model: {p.model}, served by {p.served_by or 'unknown'} at {p.endpoint_host}",
        f"- Temperature {p.temperature}, up to {p.max_output_tokens} output tokens, "
        f"context {context}, ceiling {p.ceiling}",
        f"- {p.runs_per_scenario} runs per scenario",
        f"- Commit: `{p.commit}`{dirty}",
        f"- {p.started_at} to {p.finished_at}",
        "",
    ]
    return "\n".join(lines)
