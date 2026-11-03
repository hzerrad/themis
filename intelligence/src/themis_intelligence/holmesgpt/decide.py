"""Turns HolmesGPT's answer into a suggestion, or into nothing.

Deterministic: the answer must be JSON in the schema Themis asked for, name the
incident's own Deployment, and pick a revision Themis collected that isn't the
current one. Anything else suggests nothing. No model reads HolmesGPT's text.
"""

import json
from dataclasses import dataclass
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, ValidationError

from themis.v1 import common_pb2

# The schema HolmesGPT is asked to answer in.
SCHEMA: dict[str, Any] = {
    "type": "object",
    "additionalProperties": False,
    "required": ["decision", "namespace", "deployment", "revision", "rationale", "evidence"],
    "properties": {
        "decision": {"type": "string", "enum": ["rollback", "none"]},
        "namespace": {"type": "string"},
        "deployment": {"type": "string"},
        "revision": {"type": ["integer", "null"]},
        "rationale": {"type": "string"},
        "evidence": {"type": "array", "items": {"type": "string"}},
    },
}

MAX_RATIONALE = 2000


class Answer(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)

    decision: Literal["rollback", "none"]
    namespace: str
    deployment: str
    revision: int | None
    rationale: str
    evidence: list[str]


@dataclass(frozen=True)
class Revision:
    number: int
    current: bool
    images: tuple[str, ...]


@dataclass(frozen=True)
class Suggestion:
    revision: int
    rationale: str
    cited: tuple[str, ...]


@dataclass(frozen=True)
class NoSuggestion:
    """Why HolmesGPT's answer suggests nothing, for the log."""

    reason: str


class _Container(BaseModel):
    image: str = ""


class _Revision(BaseModel):
    revision: str
    current: bool = False
    containers: list[_Container] = []


class _History(BaseModel):
    revisions: list[_Revision] = []


def revisions(items: list[common_pb2.EvidenceItem]) -> tuple[str | None, list[Revision]]:
    """The rollout history Themis collected: its evidence ID and revisions."""
    for item in items:
        if item.template != "rollout_history" or not item.data:
            continue
        try:
            history = _History.model_validate_json(item.data)
            out = [
                Revision(int(r.revision), r.current, tuple(c.image for c in r.containers))
                for r in history.revisions
            ]
        except (ValueError, ValidationError):
            return item.id, []
        return item.id, out
    return None, []


def decide(
    analysis: str,
    target: common_pb2.TargetRef,
    history_id: str | None,
    history: list[Revision],
    evidence_ids: set[str],
) -> Suggestion | NoSuggestion:
    try:
        answer = Answer.model_validate(json.loads(analysis))
    except (ValueError, ValidationError):
        return NoSuggestion("HolmesGPT's answer isn't in the format Themis asked for")
    if answer.decision == "none":
        return NoSuggestion("HolmesGPT found no rollback that would fix it")
    if target.kind != "Deployment":
        return NoSuggestion(f"Themis can only roll back a Deployment, not a {target.kind}")
    if (answer.namespace, answer.deployment) != (target.namespace, target.name):
        return NoSuggestion("HolmesGPT named another workload than the incident's")
    earlier = {r.number for r in history if not r.current}
    if answer.revision is None or answer.revision not in earlier:
        return NoSuggestion(
            "HolmesGPT picked a revision that isn't an earlier one Themis collected"
        )
    rationale = answer.rationale.strip()[:MAX_RATIONALE]
    if not rationale:
        return NoSuggestion("HolmesGPT gave no reason")
    cited = tuple(dict.fromkeys(e for e in answer.evidence if e in evidence_ids))
    # The revision was checked against the rollout history, so it supports it.
    if not cited and history_id:
        cited = (history_id,)
    return Suggestion(revision=answer.revision, rationale=rationale, cited=cited)
