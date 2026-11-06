"""Themis's own verdict on each proposal a model made.

The replay accepts every proposal, so it can't say what the core would do with
one: whether policy would let it through, with the values the model chose, and
whether it changes what the case's right fix changes. eval/ in Go decides that from
the case's cluster, as governance would, after the runs.
"""

import json
import subprocess
from pathlib import Path

from pydantic import TypeAdapter

from themis_intelligence.evaluation.score import RunRecord, Verdict

# Run from the repository's root.
GO_JUDGE = ["go", "run", "./scripts/scenario", "judge"]


class JudgeError(Exception):
    """Themis couldn't judge the proposals."""


def judge(scenarios: Path, records: list[RunRecord], command: list[str] = GO_JUDGE) -> None:
    """Sets the verdict of every proposal in records. scenarios is the
    repository's scenarios folder."""
    seen = [(r.scenario, r.proposal) for r in records if r.proposal is not None]
    if not seen:
        return
    asks = [{"scenario": s, "capability": p.capability, "params": p.params} for s, p in seen]
    done = subprocess.run(  # noqa: S603
        command,
        cwd=scenarios.resolve().parent,
        input=json.dumps(asks),
        capture_output=True,
        text=True,
        check=False,
    )
    if done.returncode != 0:
        raise JudgeError(done.stderr.strip() or f"{' '.join(command)} failed")
    verdicts = TypeAdapter(list[Verdict]).validate_json(done.stdout)
    for (_, p), v in zip(seen, verdicts, strict=True):
        p.verdict = v
