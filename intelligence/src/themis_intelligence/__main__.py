"""Runs the investigator: python -m themis_intelligence.

Configuration comes from the environment:

    THEMIS_CORE_ADDR         themis-core's gRPC address (localhost:9090)
    THEMIS_TOKEN_FILE        file holding the investigator bearer token
    MODEL_BASE_URL           an OpenAI-compatible API (http://localhost:11434/v1)
    MODEL_NAME               the model (qwen3:8b)
    MODEL_API_KEY_FILE       file holding the API key, if the endpoint needs one
    MODEL_CEILING            the most sensitive data the endpoint may see (sensitive)
    MODEL_TOKEN_BUDGET       tokens per investigation (200000)
    MODEL_PRICE_INPUT_USD_PER_MTOK, MODEL_PRICE_OUTPUT_USD_PER_MTOK
                             the route's price in US dollars per million tokens, both
                             or neither; without one, no cost is reported

Flags: --metrics-port (8082; 0 disables /metrics) and --log-level (info).
"""

import argparse
import asyncio
import os
from pathlib import Path

from themis_intelligence import telemetry, tracing
from themis_intelligence.platform.adapters.openai_compat import OpenAICompatAdapter
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
    adapter = OpenAICompatAdapter(
        base_url=env("MODEL_BASE_URL", "http://localhost:11434/v1"),
        api_key=read_secret(key_file) if key_file else None,
    )
    endpoint = Endpoint(
        name=env("MODEL_ENDPOINT", "default"),
        model=env("MODEL_NAME", "qwen3:8b"),
        ceiling=ceiling,
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
    worker = Worker(
        core=core,
        new_model=lambda: ModelClient(
            adapter=adapter, endpoint=endpoint, budget=Budget(budget), route=ROUTE, price=price
        ),
    )
    stop_tracing = tracing.setup_tracing()
    try:
        await worker.run()
    finally:
        await core.close()
        stop_tracing()


if __name__ == "__main__":
    asyncio.run(main())
