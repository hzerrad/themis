"""Runs the evaluation: python -m themis_intelligence.evaluation.

The API key, if the endpoint needs one, comes from the file --api-key-file names,
or MODEL_API_KEY_FILE. It is read once, and never logged or reported.
"""

import argparse
import asyncio
import hashlib
import os
import subprocess
from datetime import UTC, datetime
from functools import cache
from importlib import resources
from pathlib import Path

import httpx

from themis_intelligence.__main__ import read_secret
from themis_intelligence.evaluation.bundle import Bundle, bundle_dirs
from themis_intelligence.evaluation.judge import JudgeError, judge
from themis_intelligence.evaluation.recordings import (
    MissingRecordingError,
    RecordingAdapter,
    ReplayAdapter,
)
from themis_intelligence.evaluation.report import Provenance, build_report, to_markdown
from themis_intelligence.evaluation.runner import Caps, run_all
from themis_intelligence.evaluation.score import RunRecord
from themis_intelligence.investigation import loop
from themis_intelligence.investigation.schemas import SCHEMA_VERSION
from themis_intelligence.platform.adapters import PROVIDERS, connect
from themis_intelligence.platform.adapters.anthropic_messages import PRICES
from themis_intelligence.platform.budget import Budget
from themis_intelligence.platform.client import Adapter, Endpoint, ModelClient, Price
from themis_intelligence.platform.contract import CLASSIFICATION_RANK

LOCAL_HOSTS = {"localhost", "127.0.0.1", "::1"}
ROUTE = "evaluation"

# What each provider gets when a flag is left unset. A Claude model thinks
# before it answers, inside its output limit, so it needs more room; its
# effort is set rather than left to a default that differs between models.
DEFAULTS = {
    "openai": {
        "base_url": "http://127.0.0.1:11434/v1",
        "model": "qwen3:8b",
        "max_output_tokens": loop.MAX_OUTPUT_TOKENS,
        "reasoning_effort": None,
    },
    "anthropic": {
        "base_url": None,
        "model": None,
        "max_output_tokens": 16000,
        "reasoning_effort": "medium",
    },
}
ANTHROPIC_HOST = "api.anthropic.com"


def host_of(url: str) -> str:
    return httpx.URL(url).host


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(prog="themis_intelligence.evaluation")
    p.add_argument(
        "--scenarios",
        type=Path,
        default=Path("../scenarios"),
        help="the scenario packs, whose cases are in each pack's eval folder",
    )
    p.add_argument("--pack", action="append", help="run only this pack's cases; repeatable")
    p.add_argument("--scenario", action="append", help="run only this case; repeatable")
    p.add_argument("--out", type=Path, required=True, help="directory for the reports")
    p.add_argument(
        "--provider",
        choices=PROVIDERS,
        default="openai",
        help="openai: an OpenAI-compatible API, such as Ollama's; anthropic: Anthropic's",
    )
    p.add_argument("--base-url", help="the API's address; for openai, Ollama's on this machine")
    p.add_argument("--model", help="for openai, qwen3:8b; for anthropic, say which")
    p.add_argument(
        "--api-key-file",
        default=os.environ.get("MODEL_API_KEY_FILE"),
        help="file holding the API key (MODEL_API_KEY_FILE)",
    )
    p.add_argument("--runs", type=int, default=5, help="runs per scenario")
    p.add_argument("--ceiling", default="sensitive", choices=list(CLASSIFICATION_RANK))
    p.add_argument(
        "--context-tokens",
        type=int,
        help="the endpoint's context size, recorded in the report. It isn't sent: set it on the "
        "server, for Ollama with OLLAMA_CONTEXT_LENGTH",
    )
    p.add_argument(
        "--step-deadline",
        type=float,
        default=loop.STEP_DEADLINE_S,
        help="seconds a model step may take; raise it for slow local hardware",
    )
    p.add_argument(
        "--reasoning-effort",
        help="sent to reasoning models, for example none; unset: their default, "
        "or medium for anthropic",
    )
    p.add_argument(
        "--max-output-tokens",
        type=int,
        help=f"each reply's output limit ({loop.MAX_OUTPUT_TOKENS}, or "
        f"{DEFAULTS['anthropic']['max_output_tokens']} for anthropic); "
        "a model that reasons needs more",
    )
    p.add_argument(
        "--no-strict-output",
        dest="strict_output",
        action="store_false",
        help="don't ask the provider to follow the schema exactly; for OpenAI",
    )
    p.add_argument("--price-input", type=float, help="US dollars per million input tokens")
    p.add_argument("--price-output", type=float, help="US dollars per million output tokens")
    p.add_argument("--max-tokens", type=int, default=5_000_000, help="cap for the whole evaluation")
    p.add_argument("--max-usd", type=float, default=5.0, help="cap for the whole evaluation")
    saved = p.add_mutually_exclusive_group()
    saved.add_argument("--record", type=Path, help="save every model reply in this directory")
    saved.add_argument(
        "--replay", type=Path, help="answer from replies saved with --record; no model is reached"
    )
    p.add_argument(
        "--gate", action="store_true", help="exit with an error unless the safety gate passes"
    )
    args = p.parse_args(argv)
    for name, value in DEFAULTS[args.provider].items():
        if getattr(args, name) is None:
            setattr(args, name, value)
    if args.model is None:
        raise SystemExit("Set --model, such as claude-sonnet-5-5.")
    return args


def endpoint_host(args: argparse.Namespace) -> str:
    return host_of(args.base_url) if args.base_url else ANTHROPIC_HOST


def read_price(args: argparse.Namespace) -> Price | None:
    """A model that isn't on this machine must be priced, or a spending cap means nothing.
    Claude models Themis knows are priced already."""
    if args.price_input is None and args.price_output is None:
        if args.provider == "anthropic" and args.model in PRICES:
            return PRICES[args.model]
        if endpoint_host(args) not in LOCAL_HOSTS:
            raise SystemExit(
                "This endpoint isn't local: set --price-input and --price-output, "
                "so the spending cap can work."
            )
        return None
    if args.price_input is None or args.price_output is None:
        raise SystemExit("Set both --price-input and --price-output, or neither.")
    return Price(args.price_input, args.price_output)


def dataset_hash(dirs: list[Path]) -> str:
    digest = hashlib.sha256()
    for d in dirs:
        digest.update((d / "bundle.json").read_bytes())
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

    return git("rev-parse", "HEAD") or "unknown", bool(
        git("status", "--porcelain", "--untracked-files=no")
    )


def show(r: RunRecord) -> None:
    print(f"{r.scenario} #{r.run}: {r.conclusion} {r.failure}".rstrip(), flush=True)


async def main() -> None:
    args = parse_args()
    if args.runs < 1:
        raise SystemExit("--runs must be at least 1.")
    price = None if args.replay else read_price(args)
    dirs = bundle_dirs(args.scenarios, args.scenario, args.pack)
    bundles = [Bundle.model_validate_json((d / "bundle.json").read_bytes()) for d in dirs]
    if not bundles:
        raise SystemExit(f"No scenarios under {args.scenarios}.")

    live: Adapter | None = None
    if not args.replay:
        key = read_secret(args.api_key_file) if args.api_key_file else None
        live = connect(args.provider, args.base_url, key)

    # One adapter per case, kept across its runs, so its replies are its own.
    @cache
    def adapter_for(case: str) -> Adapter:
        if live is None:
            return ReplayAdapter(args.replay, case)
        if args.record:
            return RecordingAdapter(live, args.record, case)
        return live

    endpoint = Endpoint(
        name="evaluation",
        model=args.model,
        ceiling=args.ceiling,
        reasoning_effort=args.reasoning_effort,
        max_output_tokens=args.max_output_tokens,
        strict_output=args.strict_output,
    )
    caps = Caps(max_tokens=args.max_tokens, max_usd=args.max_usd, price=price)
    started = datetime.now(UTC).isoformat(timespec="seconds")
    try:
        outcome = await run_all(
            bundles,
            lambda case, tokens: ModelClient(
                adapter=adapter_for(case),
                endpoint=endpoint,
                budget=Budget(tokens),
                route=ROUTE,
                price=price,
            ),
            args.runs,
            caps,
            progress=show,
            step_deadline_s=args.step_deadline,
        )
    except MissingRecordingError:
        raise SystemExit(
            "No saved reply matches what the model would be sent now: the prompt, the schema or "
            "the evidence changed. Record new replies with `make eval-record`, and review the new "
            "numbers before committing them."
        ) from None
    try:
        judge(args.scenarios, outcome.records)
    except JudgeError as e:
        raise SystemExit(f"Themis couldn't judge the proposals: {e}") from None
    commit, dirty = git_state()
    provenance = Provenance(
        dataset=dataset_hash(dirs),
        scenario_versions={b.scenario: b.version for b in bundles},
        prompt_version=loop.PROMPT_VERSION,
        prompt_sha256=prompt_hash(),
        schema_version=SCHEMA_VERSION,
        model=args.model,
        served_by=", ".join(sorted({r.served_by for r in outcome.records if r.served_by})),
        provider=args.provider,
        endpoint_host=endpoint_host(args),
        ceiling=args.ceiling,
        # Claude models take only their own sampling settings, so none is sent.
        temperature=0 if args.provider == "openai" else None,
        max_output_tokens=args.max_output_tokens,
        reasoning_effort=args.reasoning_effort or "",
        strict_output=args.strict_output,
        step_deadline_s=args.step_deadline,
        runs_per_scenario=args.runs,
        context_tokens=args.context_tokens,
        price_input_usd_per_mtok=price.input_usd_per_mtok if price else None,
        price_output_usd_per_mtok=price.output_usd_per_mtok if price else None,
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
    if args.gate and report.safety_gate != "pass":
        raise SystemExit(f"The safety gate didn't pass ({report.safety_gate}): see {args.out}.")


if __name__ == "__main__":
    asyncio.run(main())
