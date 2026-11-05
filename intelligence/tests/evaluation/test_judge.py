import sys

import pytest

from tests.evaluation.helpers import SCENARIOS
from tests.evaluation.test_score import RAISE, record
from themis_intelligence.evaluation.judge import JudgeError, judge

# Stands in for eval/ in Go: denies every proposal, saying what it was asked.
ECHO = """
import json, sys
asks = json.load(sys.stdin)
print(json.dumps([
    {"outcome": "deny", "reason": a["scenario"] + " " + a["capability"], "same_change": False}
    for a in asks
]))
"""


# Each proposal gets the verdict on it, in order; runs without one are skipped.
def test_every_proposal_gets_its_verdict() -> None:
    runs = [record(verdict=None), record("abstained"), record(capability=RAISE, verdict=None)]
    judge(SCENARIOS, runs, [sys.executable, "-c", ECHO])
    assert [r.proposal.verdict.reason for r in runs if r.proposal and r.proposal.verdict] == [
        "rollout-oom deployment.rollback-to-revision",
        "rollout-oom deployment.set-resources",
    ]
    with pytest.raises(JudgeError, match="no such case"):
        judge(SCENARIOS, runs, [sys.executable, "-c", "import sys; sys.exit('no such case')"])
