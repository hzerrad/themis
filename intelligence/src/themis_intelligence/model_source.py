"""The investigator's model: its environment's settings where the install set
them, the settings made in Themis's setup otherwise, read before each lease so
a change in setup applies to the next investigation."""

import logging
from collections.abc import Callable
from dataclasses import dataclass, replace
from typing import Protocol, cast

from themis.v1 import investigation_pb2
from themis_intelligence.investigation.loop import Model
from themis_intelligence.platform.adapters import (
    PROVIDERS,
    Provider,
    ProviderAdapter,
    connect,
    needs_address,
)
from themis_intelligence.platform.budget import Budget
from themis_intelligence.platform.client import Endpoint
from themis_intelligence.platform.contract import SCHEMA_DIALECTS, SchemaDialect

log = logging.getLogger(__name__)


@dataclass(frozen=True)
class Installed:
    """What the install set. None leaves a setting to Themis's setup. The
    provider, the address, strict output and the schema dialect describe one
    endpoint: an address set at install brings the others with it, and
    Anthropic's address is its own, so with it setup's address doesn't apply."""

    base_url: str | None
    model: str | None
    api_key: str | None
    strict_output: bool = True
    provider: Provider | None = None
    schema_dialect: SchemaDialect | None = None


@dataclass(frozen=True)
class Resolved:
    """The model as the investigator uses it."""

    provider: Provider
    # None for Anthropic's own address.
    base_url: str | None
    api_key: str | None
    endpoint: Endpoint


def resolve(
    installed: Installed, got: investigation_pb2.GetModelSettingsResponse, template: Endpoint
) -> Resolved | None:
    """The install's settings where it made them, setup's in got otherwise, and
    template's for the rest. None while the model has no name, or no address
    when its provider needs one."""
    inst = installed
    if inst.provider is not None:
        provider = inst.provider
    elif inst.base_url:
        provider = "openai"
    else:
        provider = cast(Provider, got.provider) if got.provider in PROVIDERS else "openai"
    addressed = needs_address(provider)
    base_url = inst.base_url or (got.base_url if addressed else None) or None
    model = inst.model or got.model or None
    if not model or (addressed and not base_url):
        return None
    api_key = inst.api_key if inst.api_key is not None else (got.api_key or None)
    endpoint_by_install = bool(inst.base_url) or not addressed
    if endpoint_by_install:
        strict = inst.strict_output
    else:
        strict = got.strict_output if got.HasField("strict_output") else True
    if inst.schema_dialect is not None:
        dialect = inst.schema_dialect
    elif not endpoint_by_install and got.schema_dialect in SCHEMA_DIALECTS:
        dialect = cast(SchemaDialect, got.schema_dialect)
    else:
        dialect = "as-is"
    return Resolved(
        provider=provider,
        base_url=base_url.rstrip("/") if base_url else None,
        api_key=api_key,
        endpoint=replace(template, model=model, strict_output=strict, schema_dialect=dialect),
    )


class SettingsCore(Protocol):
    async def model_settings(self) -> investigation_pb2.GetModelSettingsResponse: ...


class ModelSource:
    def __init__(
        self,
        installed: Installed,
        core: SettingsCore,
        endpoint: Endpoint,
        make: Callable[[ProviderAdapter, Endpoint, Budget], Model],
    ) -> None:
        self.installed = installed
        self.core = core
        # The endpoint's other settings come from the environment only.
        self.template = endpoint
        self.make = make
        self.adapter: ProviderAdapter | None = None
        self.connected: tuple[Provider, str | None, str | None] | None = None
        self.endpoint: Endpoint | None = None
        self.said_missing = False

    async def ready(self) -> bool:
        """Reads the settings; False while no model is set, so no work is leased."""
        got = resolve(self.installed, await self.core.model_settings(), self.template)
        if got is None:
            if not self.said_missing:
                log.warning(
                    "no model is set, so no incident is investigated: set it on Themis's"
                    " Setup page, or with the chart's model values"
                )
                self.said_missing = True
            return False
        self.said_missing = False
        old = self.adapter
        connection = (got.provider, got.base_url, got.api_key)
        if old is None or self.connected != connection:
            self.adapter = connect(*connection)
            self.connected = connection
            if old is not None:
                await old.aclose()
        if got.endpoint != self.endpoint:
            log.info(
                "using the model",
                extra={"model": got.endpoint.model, "address": got.base_url or got.provider},
            )
            self.endpoint = got.endpoint
        return True

    def new_model(self, budget: Budget) -> Model:
        """A client for one investigation, with the settings ready() last read,
        held to budget."""
        if self.adapter is None or self.endpoint is None:
            raise RuntimeError("new_model before ready")
        return self.make(self.adapter, self.endpoint, budget)
