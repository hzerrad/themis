import pytest

from themis_intelligence.__main__ import read_flag, read_sampling


@pytest.mark.parametrize(
    ("value", "want"), [("", True), ("true", True), ("FALSE", False), (" false ", False)]
)
def test_read_flag(monkeypatch: pytest.MonkeyPatch, value: str, want: bool) -> None:
    monkeypatch.setenv("MODEL_STRICT_OUTPUT", value)
    assert read_flag("MODEL_STRICT_OUTPUT", default=True) is want


def test_read_flag_refuses_anything_else(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("MODEL_STRICT_OUTPUT", "no")
    with pytest.raises(SystemExit, match="true or false"):
        read_flag("MODEL_STRICT_OUTPUT", default=True)


# Unset, Ollama and vLLM get what they always did; OpenAI's newer models are
# set to what they accept.
@pytest.mark.parametrize(
    ("field", "temperature", "want"),
    [
        ("", "", ("max_tokens", 0.0)),
        ("max_completion_tokens", "Default", ("max_completion_tokens", None)),
        ("", "0.7", ("max_tokens", 0.7)),
    ],
)
def test_read_sampling(
    monkeypatch: pytest.MonkeyPatch, field: str, temperature: str, want: tuple[str, float | None]
) -> None:
    monkeypatch.setenv("MODEL_OUTPUT_LIMIT_FIELD", field)
    monkeypatch.setenv("MODEL_TEMPERATURE", temperature)
    assert read_sampling() == want


@pytest.mark.parametrize(
    ("name", "value", "error"),
    [("MODEL_OUTPUT_LIMIT_FIELD", "max_output", "one of"), ("MODEL_TEMPERATURE", "warm", "number")],
)
def test_read_sampling_refuses_anything_else(
    monkeypatch: pytest.MonkeyPatch, name: str, value: str, error: str
) -> None:
    monkeypatch.setenv(name, value)
    with pytest.raises(SystemExit, match=error):
        read_sampling()
