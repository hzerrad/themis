"""Compares models' reports side by side: python -m themis_intelligence.evaluation.compare.

Give it report.json files, or the folders holding them. Reports compare only
when they ran the same cases, prompt, schema and scoring; the table says when
they didn't.
"""

import argparse
from collections.abc import Callable
from pathlib import Path

from themis_intelligence.evaluation.report import Report
from themis_intelligence.evaluation.score import Rate


def _rate(r: Rate) -> str:
    return "n/a" if r.rate is None else f"{r.hits}/{r.total} ({r.rate:.0%})"


def _count(hits: int, of: int) -> str:
    return "n/a" if of == 0 else f"{hits}/{of}"


def _model(r: Report) -> str:
    p = r.provenance
    effort = f", {p.reasoning_effort}" if p.reasoning_effort else ""
    return f"{p.model}{effort}"


def _sameness(reports: list[Report]) -> list[str]:
    """What differs between the reports and makes them hard to compare."""
    fields: dict[str, Callable[[Report], str]] = {
        "cases": lambda r: r.provenance.dataset,
        "prompt": lambda r: r.provenance.prompt_sha256,
        "schema": lambda r: r.provenance.schema_version,
        "scoring": lambda r: r.provenance.scoring_version,
    }
    return [name for name, pick in fields.items() if len({pick(r) for r in reports}) > 1]


def to_table(reports: list[Report]) -> str:
    rows = [
        "| Model | Runs | Gate | Right decision | Right root cause first | Harm attempted | "
        "Harmful fix reached a person | Injection obeyed | Injection steered | "
        "Replies valid first time | Tokens in | Tokens out | Cost | Model time |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for r in reports:
        m, t = r.overall, r.twins
        cost = "not priced" if m.cost_usd is None else f"${m.cost_usd:.4f}"
        gate = r.safety_gate + (" (partial)" if r.partial else "")
        rows.append(
            f"| {_model(r)} | {m.runs} | {gate} | {_rate(m.correct_action)} | {_rate(m.top1)} | "
            f"{_rate(m.harmful)} | {_rate(m.unstopped)} | "
            f"{_count(sum(x.obeyed for x in t), len(t))} | "
            f"{_count(sum(x.steered for x in t), len(t))} | {_rate(m.clean_calls)} | "
            f"{m.input_tokens} | {m.output_tokens} | {cost} | {m.latency_ms / 1000:.0f}s |"
        )
    if differ := _sameness(reports):
        rows += ["", f"These reports don't compare: they differ in {', '.join(differ)}."]
    return "\n".join(rows) + "\n"


def load(paths: list[Path]) -> list[Report]:
    files = [p / "report.json" if p.is_dir() else p for p in paths]
    return [Report.model_validate_json(f.read_bytes()) for f in files]


def main(argv: list[str] | None = None) -> None:
    p = argparse.ArgumentParser(prog="themis_intelligence.evaluation.compare")
    p.add_argument("reports", type=Path, nargs="+", help="report.json files, or their folders")
    args = p.parse_args(argv)
    print(to_table(load(args.reports)), end="")


if __name__ == "__main__":
    main()
