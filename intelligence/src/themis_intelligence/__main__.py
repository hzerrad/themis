"""Runs the investigator: python -m themis_intelligence.

Configuration comes from the environment:

    THEMIS_CORE_ADDR         themis-core's gRPC address (localhost:9090)
    THEMIS_TOKEN_FILE        file holding the investigator bearer token
    MODEL_BASE_URL           an OpenAI-compatible API (http://localhost:11434/v1)
    MODEL_NAME               the model (qwen3:8b)
    MODEL_API_KEY_FILE       file holding the API key, if the endpoint needs one
    MODEL_CEILING            the most sensitive data the endpoint may see (sensitive)
    MODEL_TOKEN_BUDGET       tokens per investigation (200000)

Flags: --log-level (info).
"""

import argparse
import asyncio
import os
from pathlib import Path

from themis_intelligence import telemetry
from themis_intelligence.platform.adapters.openai_compat import OpenAICompatAdapter
from themis_intelligence.platform.budget import Budget
from themis_intelligence.platform.client import Endpoint, ModelClient
from themis_intelligence.platform.contract import CLASSIFICATION_RANK
from themis_intelligence.rpc.core_client import CoreClient
from themis_intelligence.worker import Worker


def read_secret(path: str) -> str:
    token = Path(path).read_text().strip()
    if not token:
        raise SystemExit(f"{path} is empty")
    return token


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(prog="themis_intelligence")
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
        raise SystemExit(f"MODEL_CEILING must be one of {', '.join(CLASSIFICATION_RANK)}")
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

    core = CoreClient(
        env("THEMIS_CORE_ADDR", "localhost:9090"),
        read_secret(env("THEMIS_TOKEN_FILE", "/var/run/secrets/themis/investigator-token")),
    )
    worker = Worker(
        core=core,
        new_model=lambda: ModelClient(adapter=adapter, endpoint=endpoint, budget=Budget(budget)),
    )
    try:
        await worker.run()
    finally:
        await core.close()


if __name__ == "__main__":
    asyncio.run(main())
