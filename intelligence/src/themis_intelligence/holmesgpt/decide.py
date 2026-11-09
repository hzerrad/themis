"""Turns HolmesGPT's answer into a suggestion, or into nothing.

Deterministic: the answer must be JSON in the schema Themis asked for, name the
incident's own workload, and choose one of the fixes the incident offers, with
parameters that fit that fix's schema. Anything else suggests nothing. No
model reads HolmesGPT's text. These checks only keep nonsense from reaching
Themis, which judges every suggestion itself.
"""

import json
from collections.abc import Sequence
from dataclasses import dataclass
from typing import Any, cast

from jsonschema import Draft202012Validator, SchemaError, validate
from jsonschema import ValidationError as Unfit
from pydantic import BaseModel, ConfigDict, ValidationError

from themis.v1 import common_pb2

# What HolmesGPT answers when no fix on offer would fix the incident. Fix
# names have a dot, so it can't be one.
NONE = "none"

MAX_RATIONALE = 2000
MAX_EFFECT = 500
# A name HolmesGPT made up is shortened to this in the log.
MAX_NAME = 100


@dataclass(frozen=True)
class Fix:
    """A fix the incident offers HolmesGPT, as GetIncident describes it."""

    name: str
    description: str
    # The JSON Schema of its parameters; None if Themis sent none readable.
    schema: dict[str, Any] | None


def offered(capabilities: Sequence[common_pb2.Capability]) -> list[Fix]:
    return [Fix(c.name, c.description, readable(c.params_schema)) for c in capabilities]


def readable(text: str) -> dict[str, Any] | None:
    """A parameter schema, if it is one."""
    try:
        schema = json.loads(text)
        Draft202012Validator.check_schema(schema)
    except (ValueError, SchemaError):
        return None
    return cast(dict[str, Any], schema) if isinstance(schema, dict) else None


def verdict_schema(fixes: list[Fix], evidence_ids: list[str]) -> dict[str, Any]:
    """The schema HolmesGPT is asked to answer in. Every provider's structured
    output takes it: each property is required and none is a free-form
    object, so the parameters are a string holding their JSON."""
    cited: dict[str, Any] = {"type": "string"}
    if evidence_ids:
        cited["enum"] = evidence_ids
    properties: dict[str, Any] = {
        "fix": {"type": "string", "enum": [f.name for f in fixes] + [NONE]},
        "params": {"type": "string"},
        "namespace": {"type": "string"},
        "workload": {"type": "string"},
        "rationale": {"type": "string"},
        "expected_effect": {"type": "string"},
        "evidence": {"type": "array", "items": cited},
    }
    return {
        "type": "object",
        "additionalProperties": False,
        "required": list(properties),
        "properties": properties,
    }


class Answer(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)

    fix: str
    # A string holding a JSON object, as asked; an object as such is taken too.
    params: str | dict[str, Any]
    namespace: str
    workload: str
    rationale: str
    expected_effect: str
    evidence: list[str]


@dataclass(frozen=True)
class Suggestion:
    fix: str
    params: dict[str, Any]
    rationale: str
    expected_effect: str
    cited: tuple[str, ...]


@dataclass(frozen=True)
class NoSuggestion:
    """Why HolmesGPT's answer suggests nothing, for the log."""

    reason: str


def decide(
    analysis: str,
    target: common_pb2.TargetRef,
    fixes: list[Fix],
    evidence_ids: set[str],
) -> Suggestion | NoSuggestion:
    try:
        answer = Answer.model_validate(json.loads(analysis))
    except (ValueError, ValidationError):
        return NoSuggestion("HolmesGPT's answer isn't in the format Themis asked for")
    if answer.fix == NONE:
        return NoSuggestion("HolmesGPT chose none of the fixes on offer")
    if (answer.namespace, answer.workload) != (target.namespace, target.name):
        return NoSuggestion("HolmesGPT named another workload than the incident's")
    fix = next((f for f in fixes if f.name == answer.fix), None)
    if fix is None:
        return NoSuggestion(
            f"HolmesGPT chose {answer.fix[:MAX_NAME]!r}, which the incident doesn't offer"
        )
    params, wrong = parameters(answer.params, fix)
    if wrong:
        return NoSuggestion(f"HolmesGPT's parameters for {fix.name} {wrong}")
    rationale = answer.rationale.strip()[:MAX_RATIONALE]
    if not rationale:
        return NoSuggestion("HolmesGPT gave no reason")
    cited = tuple(dict.fromkeys(e for e in answer.evidence if e in evidence_ids))
    if not cited:
        return NoSuggestion("HolmesGPT cited no evidence Themis collected")
    return Suggestion(
        fix=fix.name,
        params=params,
        rationale=rationale,
        expected_effect=answer.expected_effect.strip()[:MAX_EFFECT],
        cited=cited,
    )


def parameters(given: str | dict[str, Any], fix: Fix) -> tuple[dict[str, Any], str]:
    """The parameters HolmesGPT gave, or what is wrong with them, to follow
    "HolmesGPT's parameters for <fix> "."""
    if isinstance(given, str):
        try:
            given = json.loads(given)
        except ValueError:
            return {}, "aren't JSON"
    if not isinstance(given, dict):
        return {}, "aren't a JSON object"
    if fix.schema is None:
        return {}, "can't be checked, since Themis sent no schema the adapter can read"
    try:
        validate(given, fix.schema, cls=Draft202012Validator)
    except Unfit as e:
        return {}, f"don't fit its schema: {e.message}"
    return given, ""
