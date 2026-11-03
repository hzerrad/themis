"""The investigator's model: its environment's settings where the install set
them, the settings made in Themis's setup otherwise, read before each lease so
a change in setup applies to the next investigation."""

import logging
from collections.abc import Callable
from dataclasses import dataclass, replace
from typing import Protocol

from themis.v1 import investigation_pb2
from themis_intelligence.investigation.loop import Model
from themis_intelligence.platform.adapters.openai_compat import OpenAICompatAdapter
from themis_intelligence.platform.client import Endpoint

log = logging.getLogger(__name__)


@dataclass(frozen=True)
class Installed:
    """What the install set. None leaves a setting to Themis's setup. Strict
    output goes with the address: both describe the same endpoint."""

    base_url: str | None
    model: str | None
    api_key: str | None
    strict_output: bool = True


class SettingsCore(Protocol):
    async def model_settings(self) -> investigation_pb2.GetModelSettingsResponse: ...


class ModelSource:
    def __init__(
        self,
        installed: Installed,
        core: SettingsCore,
        endpoint: Endpoint,
        make: Callable[[OpenAICompatAdapter, Endpoint], Model],
    ) -> None:
        self.installed = installed
        self.core = core
        # The endpoint's other settings come from the environment only.
        self.template = endpoint
        self.make = make
        self.adapter: OpenAICompatAdapter | None = None
        self.endpoint: Endpoint | None = None
        self.said_missing = False

    async def ready(self) -> bool:
        """Reads the settings; False while no model is set, so no work is leased."""
        got = await self.core.model_settings()
        inst = self.installed
        base_url = inst.base_url or got.base_url or None
        model = inst.model or got.model or None
        if not base_url or not model:
            if not self.said_missing:
                log.warning(
                    "no model is set, so no incident is investigated: set it on Themis's"
                    " Setup page, or with the chart's model values"
                )
                self.said_missing = True
            return False
        self.said_missing = False
        api_key = inst.api_key if inst.api_key is not None else (got.api_key or None)
        if inst.base_url:
            strict = inst.strict_output
        else:
            strict = got.strict_output if got.HasField("strict_output") else True
        endpoint = replace(self.template, model=model, strict_output=strict)
        old = self.adapter
        if old is None or (old.base_url, old.api_key) != (base_url.rstrip("/"), api_key):
            self.adapter = OpenAICompatAdapter(base_url=base_url, api_key=api_key)
            if old is not None:
                await old.client.aclose()
        if endpoint != self.endpoint:
            log.info("using the model", extra={"model": model, "address": base_url})
            self.endpoint = endpoint
        return True

    def new_model(self) -> Model:
        """A client for one investigation, with the settings ready() last read."""
        if self.adapter is None or self.endpoint is None:
            raise RuntimeError("new_model before ready")
        return self.make(self.adapter, self.endpoint)
