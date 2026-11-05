"""Model provider adapters, and the provider each speaks to."""

from typing import Literal, get_args

from themis_intelligence.platform.adapters.anthropic_messages import AnthropicAdapter
from themis_intelligence.platform.adapters.openai_compat import OpenAICompatAdapter

# openai: any OpenAI-compatible API, such as Ollama's, vLLM's or OpenAI's own.
# anthropic: Anthropic's Messages API.
Provider = Literal["openai", "anthropic"]
PROVIDERS: tuple[str, ...] = get_args(Provider)

ProviderAdapter = OpenAICompatAdapter | AnthropicAdapter


def needs_address(provider: Provider) -> bool:
    """Anthropic has one address, the SDK's own; an OpenAI-compatible API has many."""
    return provider == "openai"


def connect(provider: Provider, base_url: str | None, api_key: str | None) -> ProviderAdapter:
    if provider == "anthropic":
        return AnthropicAdapter(api_key=api_key, base_url=base_url)
    if not base_url:
        raise ValueError("an OpenAI-compatible provider needs an address")
    return OpenAICompatAdapter(base_url=base_url, api_key=api_key)
