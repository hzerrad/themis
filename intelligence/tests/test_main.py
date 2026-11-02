import pytest

from themis_intelligence.__main__ import read_flag


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
