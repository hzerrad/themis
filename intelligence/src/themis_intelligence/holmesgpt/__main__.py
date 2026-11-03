"""Runs the HolmesGPT adapter: python -m themis_intelligence.holmesgpt.

Configuration comes from the environment:

    THEMIS_CORE_ADDR      themis-core's gRPC address (themis-core:9090)
    THEMIS_TOKEN_FILE     a service account token issued for the audience themis,
                          read on every call
    HOLMES_URL            HolmesGPT's HTTP API, such as http://holmes-holmes.holmes
    HOLMES_API_KEY_FILE   file holding HolmesGPT's API key, if it has one
    HOLMES_MODEL          a key of HolmesGPT's modelList; unset uses its default
    POLL_SECONDS          how often to look for incidents waiting for a suggestion (15)
"""

import asyncio
import logging
import os
from pathlib import Path

from themis_intelligence import telemetry
from themis_intelligence.holmesgpt.agent import Agent, ThemisClient
from themis_intelligence.holmesgpt.holmes import Holmes

log = logging.getLogger(__name__)


def read(path: str) -> str:
    return Path(path).read_text().strip()


async def main() -> None:
    telemetry.configure_logging(os.environ.get("LOG_LEVEL", "INFO").upper())
    env = os.environ.get
    url = env("HOLMES_URL")
    if not url:
        raise SystemExit("Set HOLMES_URL to HolmesGPT's HTTP API.")
    token_file = env("THEMIS_TOKEN_FILE", "/var/run/secrets/themis/token")
    key_file = env("HOLMES_API_KEY_FILE")
    holmes = Holmes(url, read(key_file) if key_file else None, env("HOLMES_MODEL") or None)
    themis = ThemisClient(env("THEMIS_CORE_ADDR", "themis-core:9090"), lambda: read(token_file))
    if not key_file:
        log.warning("HolmesGPT is used without an API key: anyone who can reach it can use it")
    try:
        await Agent(themis, holmes, float(env("POLL_SECONDS", "15"))).run()
    finally:
        await themis.close()
        await holmes.close()


if __name__ == "__main__":
    asyncio.run(main())
