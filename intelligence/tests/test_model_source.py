"""The investigator's model: the install's settings first, setup's otherwise."""

from typing import Any, cast

import pytest

from themis.v1 import investigation_pb2
from themis_intelligence.investigation.loop import Model
from themis_intelligence.model_source import Installed, ModelSource
from themis_intelligence.platform.adapters import ProviderAdapter
from themis_intelligence.platform.adapters.anthropic_messages import AnthropicAdapter
from themis_intelligence.platform.budget import Budget
from themis_intelligence.platform.client import Endpoint


class SetupCore:
    def __init__(self, **settings: Any) -> None:
        self.settings = investigation_pb2.GetModelSettingsResponse(**settings)

    async def model_settings(self) -> investigation_pb2.GetModelSettingsResponse:
        return self.settings


class Made:
    """Records what each model client was made with."""

    def __init__(self) -> None:
        self.adapter: ProviderAdapter | None = None
        self.endpoint: Endpoint | None = None

    def __call__(self, adapter: ProviderAdapter, endpoint: Endpoint, budget: Budget) -> Model:
        self.adapter, self.endpoint = adapter, endpoint
        return cast(Model, object())


def source(installed: Installed, core: SetupCore) -> tuple[ModelSource, Made]:
    made = Made()
    template = Endpoint(name="default", model="", ceiling="sensitive", max_output_tokens=4096)
    return ModelSource(installed, core, template, made), made


NOTHING = Installed(base_url=None, model=None, api_key=None)


async def test_nothing_set_leases_nothing() -> None:
    s, _ = source(NOTHING, SetupCore())
    assert not await s.ready()


async def test_setup_fills_what_the_install_left() -> None:
    core = SetupCore(
        base_url="https://api.openai.com/v1", model="gpt-5", api_key="sk", strict_output=False
    )
    s, made = source(NOTHING, core)
    assert await s.ready()
    s.new_model(Budget(1))
    assert made.adapter is not None and made.endpoint is not None
    assert (made.adapter.base_url, made.adapter.api_key) == ("https://api.openai.com/v1", "sk")
    assert (made.endpoint.model, made.endpoint.strict_output) == ("gpt-5", False)
    assert made.endpoint.max_output_tokens == 4096


async def test_the_install_wins() -> None:
    installed = Installed(base_url="http://ollama:11434/v1", model=None, api_key=None)
    core = SetupCore(base_url="https://elsewhere/v1", model="qwen3:8b", strict_output=False)
    s, made = source(installed, core)
    assert await s.ready()
    s.new_model(Budget(1))
    assert made.adapter is not None and made.endpoint is not None
    # The address and its strict output are the install's; the name is setup's.
    assert made.adapter.base_url == "http://ollama:11434/v1"
    assert (made.endpoint.model, made.endpoint.strict_output) == ("qwen3:8b", True)


async def test_a_change_in_setup_applies_to_the_next_investigation() -> None:
    core = SetupCore(base_url="http://a/v1", model="m1")
    s, made = source(NOTHING, core)
    assert await s.ready()
    s.new_model(Budget(1))
    first = made.adapter
    core.settings = investigation_pb2.GetModelSettingsResponse(base_url="http://a/v1", model="m2")
    assert await s.ready()
    s.new_model(Budget(1))
    assert made.adapter is first, "the same address and key keep the same connection"
    assert made.endpoint is not None and made.endpoint.model == "m2"
    core.settings = investigation_pb2.GetModelSettingsResponse(
        base_url="http://b/v1", model="m2", api_key="k"
    )
    assert await s.ready()
    s.new_model(Budget(1))
    assert made.adapter is not first
    assert made.adapter is not None and (made.adapter.base_url, made.adapter.api_key) == (
        "http://b/v1",
        "k",
    )


# Anthropic's address is its own: setup's, made for another provider, doesn't apply.
async def test_anthropic_needs_only_a_model() -> None:
    installed = Installed(base_url=None, model=None, api_key=None, provider="anthropic")
    core = SetupCore(base_url="http://ollama:11434/v1", model="claude-sonnet-5-5", api_key="k")
    s, made = source(installed, core)
    assert await s.ready()
    s.new_model(Budget(1))
    assert isinstance(made.adapter, AnthropicAdapter)
    assert (made.adapter.base_url, made.adapter.api_key) == (None, "k")


# What the install leaves unset, setup chooses: the provider, and for an
# OpenAI-compatible API how the reply schema is sent. An address set at install
# keeps the install's.
async def test_setup_chooses_the_provider_and_the_dialect() -> None:
    s, made = source(NOTHING, SetupCore(provider="anthropic", base_url="http://x/v1", model="m"))
    assert await s.ready()
    s.new_model()
    assert isinstance(made.adapter, AnthropicAdapter) and made.adapter.base_url is None

    core = SetupCore(base_url="http://a/v1", model="m", schema_dialect="openai-strict")
    s, made = source(NOTHING, core)
    assert await s.ready()
    s.new_model()
    assert made.endpoint is not None and made.endpoint.schema_dialect == "openai-strict"

    installed = Installed(base_url="http://ollama:11434/v1", model=None, api_key=None)
    core = SetupCore(provider="anthropic", model="m", schema_dialect="openai-strict")
    s, made = source(installed, core)
    assert await s.ready()
    s.new_model()
    assert made.adapter is not None and made.adapter.base_url == "http://ollama:11434/v1"
    assert made.endpoint is not None and made.endpoint.schema_dialect == "as-is"


def test_new_model_before_ready() -> None:
    s, _ = source(NOTHING, SetupCore())
    with pytest.raises(RuntimeError):
        s.new_model(Budget(1))
