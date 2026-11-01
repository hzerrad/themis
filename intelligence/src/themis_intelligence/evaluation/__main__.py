"""Runs the evaluation: python -m themis_intelligence.evaluation.

The API key, if the endpoint needs one, comes from a file named by MODEL_API_KEY_FILE.
"""

import argparse
import asyncio
import hashlib
import os
import subprocess
from datetime import UTC, datetime
from importlib import resources
from pathlib import Path

import httpx

from themis_intelligence.__main__ import read_secret
from themis_intelligence.evaluation.bundle import Bundle, load_bundles
from themis_intelligence.evaluation.report import Provenance, build_report, to_markdown
from themis_intelligence.evaluation.runner import Caps, run_all
from themis_intelligence.evaluation.score import RunRecord
from themis_intelligence.investigation import loop
from themis_intelligence.investigation.schemas import SCHEMA_VERSION
from themis_intelligence.platform.adapters.openai_compat import OpenAICompatAdapter
from themis_intelligence.platform.budget import Budget
from themis_intelligence.platform.client import Endpoint, ModelClient, Price
from themis_intelligence.platform.contract import CLASSIFICATION_RANK

LOCAL_HOSTS = {"localhost", "127.0.0.1", "::1"}
ROUTE = "evaluation"


def host_of(url: str) -> str:
    return httpx.URL(url).host


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(prog="themis_intelligence.evaluation")
    p.add_argument("--scenarios", type=Path, default=Path("../eval/scenarios"))
    p.add_argument("--scenario", action="append", help="run only this scenario; repeatable")
    p.add_argument("--out", type=Path, required=True, help="directory for the reports")
    p.add_argument("--base-url", default="http://127.0.0.1:11434/v1")
    p.add_argument("--model", default="qwen3:8b")
    p.add_argument("--runs", type=int, default=5, help="runs per scenario")
    p.add_argument("--ceiling", default="sensitive", choices=list(CLASSIFICATION_RANK))
    p.add_argument(
        "--context-tokens",
        type=int,
        help="the endpoint's context size, recorded in the report. It isn't sent: set it on the "
        "server, for Ollama with OLLAMA_CONTEXT_LENGTH",
    )
    p.add_argument("--price-input", type=float, help="US dollars per million input tokens")
    p.add_argument("--price-output", type=float, help="US dollars per million output tokens")
    p.add_argument("--max-tokens", type=int, default=5_000_000, help="cap for the whole evaluation")
    p.add_argument("--max-usd", type=float, default=5.0, help="cap for the whole evaluation")
    return p.parse_args(argv)


def read_price(args: argparse.Namespace) -> Price | None:
    """A model that isn't on this machine must be priced, or a spending cap means nothing."""
    if args.price_input is None and args.price_output is None:
        if host_of(args.base_url) not in LOCAL_HOSTS:
            raise SystemExit(
                "This endpoint isn't local: set --price-input and --price-output, "
                "so the spending cap can work."
            )
        return None
    if args.price_input is None or args.price_output is None:
        raise SystemExit("Set both --price-input and --price-output, or neither.")
    return Price(args.price_input, args.price_output)


def dataset_hash(root: Path, bundles: list[Bundle]) -> str:
    digest = hashlib.sha256()
    for b in bundles:
        digest.update((root / b.scenario / "bundle.json").read_bytes())
    return digest.hexdigest()


def prompt_hash() -> str:
    path = resources.files(loop.__package__).joinpath("prompts", f"{loop.PROMPT_VERSION}.md")
    return hashlib.sha256(path.read_bytes()).hexdigest()


def git_state() -> tuple[str, bool]:
    def git(*args: str) -> str:
        return subprocess.run(  # noqa: S603
            ["git", *args],  # noqa: S607
            capture_output=True,
            text=True,
            check=False,
        ).stdout.strip()

    return git("rev-parse", "HEAD") or "unknown", bool(git("status", "--porcelain"))


def show(r: RunRecord) -> None:
    print(f"{r.scenario} #{r.run}: {r.conclusion} {r.failure}".rstrip(), flush=True)


async def main() -> None:
    args = parse_args()
    if args.runs < 1:
        raise SystemExit("--runs must be at least 1.")
    price = read_price(args)
    bundles = load_bundles(args.scenarios, args.scenario)
    if not bundles:
        raise SystemExit(f"No scenarios under {args.scenarios}.")

    key_file = os.environ.get("MODEL_API_KEY_FILE")
    adapter = OpenAICompatAdapter(
        base_url=args.base_url, api_key=read_secret(key_file) if key_file else None
    )
    endpoint = Endpoint(name="evaluation", model=args.model, ceiling=args.ceiling)
    caps = Caps(max_tokens=args.max_tokens, max_usd=args.max_usd, price=price)
    started = datetime.now(UTC).isoformat(timespec="seconds")
    outcome = await run_all(
        bundles,
        lambda tokens: ModelClient(
            adapter=adapter, endpoint=endpoint, budget=Budget(tokens), route=ROUTE, price=price
        ),
        args.runs,
        caps,
        progress=show,
    )
    commit, dirty = git_state()
    provenance = Provenance(
        dataset=dataset_hash(args.scenarios, bundles),
        scenario_versions={b.scenario: b.version for b in bundles},
        prompt_version=loop.PROMPT_VERSION,
        prompt_sha256=prompt_hash(),
        schema_version=SCHEMA_VERSION,
        model=args.model,
        served_by=", ".join(sorted({r.served_by for r in outcome.records if r.served_by})),
        endpoint_host=host_of(args.base_url),
        ceiling=args.ceiling,
        temperature=0,
        max_output_tokens=loop.MAX_OUTPUT_TOKENS,
        runs_per_scenario=args.runs,
        context_tokens=args.context_tokens,
        price_input_usd_per_mtok=args.price_input,
        price_output_usd_per_mtok=args.price_output,
        max_tokens=args.max_tokens,
        max_usd=args.max_usd if price is not None else None,
        commit=commit,
        dirty=dirty,
        started_at=started,
        finished_at=datetime.now(UTC).isoformat(timespec="seconds"),
    )
    report = build_report(provenance, bundles, outcome.records, outcome.stopped)
    args.out.mkdir(parents=True, exist_ok=True)
    (args.out / "report.json").write_text(report.model_dump_json(indent=2) + "\n")
    (args.out / "report.md").write_text(to_markdown(report))
    print(f"Safety gate: {report.safety_gate}. Reports are in {args.out}.")
    if report.partial:
        print(report.partial_reason)


if __name__ == "__main__":
    asyncio.run(main())
