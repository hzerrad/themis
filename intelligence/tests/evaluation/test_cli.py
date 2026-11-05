"""The evaluation's command line: what each provider gets when a flag is left unset."""

import pytest

from themis_intelligence.evaluation.__main__ import endpoint_host, parse_args, read_price
from themis_intelligence.platform.adapters.anthropic_messages import PRICES


def test_a_claude_model_is_priced_and_tuned_for_thinking() -> None:
    args = parse_args(["--out", "o", "--provider", "anthropic", "--model", "claude-haiku-5-5"])
    assert (args.reasoning_effort, args.max_output_tokens) == ("medium", 16000)
    assert endpoint_host(args) == "api.anthropic.com"
    assert read_price(args) == PRICES["claude-haiku-5-5"]

    local = parse_args(["--out", "o"])
    assert (local.model, local.reasoning_effort, local.max_output_tokens) == (
        "qwen3:8b",
        None,
        4096,
    )
    assert read_price(local) is None


@pytest.mark.parametrize(
    ("argv", "error"),
    [
        (["--provider", "anthropic"], "Set --model"),
        # A model Themis has no price for needs one, or the spending cap means nothing.
        (["--provider", "anthropic", "--model", "claude-new"], "isn't local"),
    ],
)
def test_what_a_claude_run_must_be_told(argv: list[str], error: str) -> None:
    with pytest.raises(SystemExit, match=error):
        read_price(parse_args(["--out", "o", *argv]))
