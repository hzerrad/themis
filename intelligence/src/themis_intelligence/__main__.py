"""Runs the investigator: python -m themis_intelligence.

Configuration comes from the environment:

    THEMIS_CORE_ADDR         themis-core's gRPC address (localhost:9090)
    THEMIS_TOKEN_FILE        file holding the investigator bearer token
    MODEL_PROVIDER           openai (the default): an OpenAI-compatible API, as Ollama,
                             vLLM and OpenAI serve; or anthropic: Anthropic's Messages API
    MODEL_BASE_URL           the API's address, such as http://localhost:11434/v1; for
                             anthropic, unset uses Anthropic's
    MODEL_NAME               the model, such as qwen3:8b
    MODEL_API_KEY_FILE       file holding the API key, if the endpoint needs one
                             The three above, and MODEL_STRICT_OUTPUT with the address,
                             come from Themis's setup when unset here; without an
                             address and a name, no incident is investigated
    MODEL_CEILING            the most sensitive data the endpoint may see (sensitive)
    MODEL_TOKEN_BUDGET       tokens per investigation (200000)
    MODEL_REASONING_EFFORT   sent to reasoning models, for example none or low (for
                             anthropic: low, medium, high, xhigh or max); unset leaves
                             the provider's default
    MODEL_MAX_OUTPUT_TOKENS  each reply's output limit (4096); raise it for a model that
                             reasons, or its replies are cut off
    MODEL_STRICT_OUTPUT      true or false (true): ask the provider to follow the reply
                             schema exactly; set false for OpenAI, which refuses it
    MODEL_PRICE_INPUT_USD_PER_MTOK, MODEL_PRICE_OUTPUT_USD_PER_MTOK
                             the route's price in US dollars per million tokens, both
                             or neither; without one, no cost is reported

Flags: --metrics-port (8082; 0 disables /metrics) and --log-level (info).
"""

import argparse
import asyncio
import os
from pathlib import Path
from typing import cast

from themis_intelligence import telemetry, tracing
from themis_intelligence.model_source import Installed, ModelSource
from themis_intelligence.platform.adapters import PROVIDERS, Provider
from themis_intelligence.platform.budget import Budget
from themis_intelligence.platform.client import Endpoint, ModelClient, Price
from themis_intelligence.platform.contract import CLASSIFICATION_RANK
from themis_intelligence.rpc.core_client import CoreClient
from themis_intelligence.worker import Worker


def read_secret(path: str) -> str:
    token = Path(path).read_text().strip()
    if not token:
        raise SystemExit(f"{path} is empty, but it must hold a secret.")
    return token


ROUTE = "default"


def read_flag(name: str, default: bool) -> bool:
    value = os.environ.get(name, "").strip().lower()
    if value == "":
        return default
    if value not in ("true", "false"):
        raise SystemExit(f"{name} must be true or false.")
    return value == "true"


def read_price() -> Price | None:
    env = os.environ.get
    prices = env("MODEL_PRICE_INPUT_USD_PER_MTOK"), env("MODEL_PRICE_OUTPUT_USD_PER_MTOK")
    if prices == (None, None):
        return None
    if None in prices:
        raise SystemExit(
            "Set both MODEL_PRICE_INPUT_USD_PER_MTOK and MODEL_PRICE_OUTPUT_USD_PER_MTOK,"
            " or neither."
        )
    try:
        return Price(float(prices[0] or ""), float(prices[1] or ""))
    except ValueError:
        raise SystemExit("Model prices must be numbers: US dollars per million tokens.") from None


def read_provider() -> Provider:
    value = os.environ.get("MODEL_PROVIDER", "").strip().lower() or "openai"
    if value not in PROVIDERS:
        raise SystemExit(f"MODEL_PROVIDER must be one of {', '.join(PROVIDERS)}.")
    return cast(Provider, value)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(prog="themis_intelligence")
    p.add_argument("--metrics-port", type=int, default=8082, help="port of /metrics; 0 disables it")
    p.add_argument(
        "--log-level",
        default="info",
        choices=["debug", "info", "warn", "error"],
        help="least severe level logged",
    )
    return p.parse_args(argv)


async def main() -> None:
    args = parse_args()
    telemetry.configure_logging("WARNING" if args.log_level == "warn" else args.log_level.upper())
    env = os.environ.get
    ceiling = env("MODEL_CEILING", "sensitive")
    if ceiling not in CLASSIFICATION_RANK:
        raise SystemExit(f"MODEL_CEILING must be one of {', '.join(CLASSIFICATION_RANK)}.")
    key_file = env("MODEL_API_KEY_FILE")
    # What the install left unset comes from Themis's setup, read before each lease.
    installed = Installed(
        base_url=env("MODEL_BASE_URL") or None,
        model=env("MODEL_NAME") or None,
        api_key=read_secret(key_file) if key_file else None,
        strict_output=read_flag("MODEL_STRICT_OUTPUT", default=True),
        provider=read_provider(),
    )
    max_output = env("MODEL_MAX_OUTPUT_TOKENS")
    endpoint = Endpoint(
        name=env("MODEL_ENDPOINT", "default"),
        model="",
        ceiling=ceiling,
        reasoning_effort=env("MODEL_REASONING_EFFORT") or None,
        max_output_tokens=int(max_output) if max_output else None,
    )
    budget = int(env("MODEL_TOKEN_BUDGET", "200000"))
    price = read_price()
    telemetry.configure_routes([ROUTE])
    telemetry.init_route(ROUTE, priced=price is not None)
    telemetry.serve_metrics(args.metrics_port)

    core = CoreClient(
        env("THEMIS_CORE_ADDR", "localhost:9090"),
        read_secret(env("THEMIS_TOKEN_FILE", "/var/run/secrets/themis/investigator-token")),
    )
    source = ModelSource(
        installed,
        core,
        endpoint,
        lambda adapter, endpoint: ModelClient(
            adapter=adapter, endpoint=endpoint, budget=Budget(budget), route=ROUTE, price=price
        ),
    )
    worker = Worker(core=core, new_model=source.new_model, ready=source.ready)
    stop_tracing = tracing.setup_tracing()
    try:
        await worker.run()
    finally:
        await core.close()
        stop_tracing()


if __name__ == "__main__":
    asyncio.run(main())
