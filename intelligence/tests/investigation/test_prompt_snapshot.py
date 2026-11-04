"""The exact first request the model gets, kept as text so a prompt change shows
as a readable diff in review. After changing the prompt on purpose, rewrite it
with THEMIS_UPDATE_SNAPSHOTS=1 and read the diff."""

import json
import os
from pathlib import Path

from themis_intelligence.evaluation.bundle import job_of, load_bundles
from themis_intelligence.investigation.loop import PROMPT_VERSION, incident_prompt, system_prompt
from themis_intelligence.investigation.schemas import SCHEMA_VERSION, reply_schema

ROOT = Path(__file__).parents[3]
SNAPSHOT = Path(__file__).parent / "golden" / "rollout-oom-first-request.txt"


def first_request() -> str:
    b = load_bundles(ROOT / "scenarios", ["rollout-oom"])[0]
    job = job_of(b)
    # The schema the loop sends: narrowed to the fixes on offer.
    schema = json.dumps(reply_schema(job.capabilities), indent=2, sort_keys=True)
    return (
        f"# prompt {PROMPT_VERSION}, schema {SCHEMA_VERSION}\n\n"
        f"## system\n\n{system_prompt(job, b.max_steps)}\n\n"
        f"## user\n\n{incident_prompt(job)}\n\n"
        f"## response schema\n\n{schema}\n"
    )


def test_the_first_request_is_the_reviewed_one() -> None:
    got = first_request()
    if os.environ.get("THEMIS_UPDATE_SNAPSHOTS"):
        SNAPSHOT.write_text(got)
    assert SNAPSHOT.read_text() == got, (
        "The model's first request changed. If on purpose, rerun with "
        "THEMIS_UPDATE_SNAPSHOTS=1 and review the diff of "
        f"{SNAPSHOT.relative_to(ROOT)}."
    )
