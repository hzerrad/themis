from pathlib import Path

import pytest

from tests.evaluation.helpers import oom
from tests.evaluation.test_report import provenance
from tests.evaluation.test_score import record
from themis_intelligence.evaluation.compare import main
from themis_intelligence.evaluation.report import build_report


def test_models_side_by_side(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    for name, p, runs in (
        ("qwen", provenance(model="qwen3:8b"), [record(revision=9)]),
        ("claude", provenance(model="claude-sonnet-5-5", reasoning_effort="medium"), [record()]),
        ("newer", provenance(model="claude-haiku-5-5", prompt_sha256="0" * 64), [record()]),
    ):
        (tmp_path / name).mkdir()
        report = build_report(p, [oom()], runs, "")
        (tmp_path / name / "report.json").write_text(report.model_dump_json())

    main([str(tmp_path / "qwen"), str(tmp_path / "claude" / "report.json")])
    qwen, claude = capsys.readouterr().out.splitlines()[2:]
    assert qwen.startswith("| qwen3:8b | 1 | fail | 0/1 (0%) |")
    assert claude.startswith("| claude-sonnet-5-5, medium | 1 | pass | 1/1 (100%) |")

    main([str(tmp_path / "claude"), str(tmp_path / "newer")])
    assert "they differ in prompt" in capsys.readouterr().out
