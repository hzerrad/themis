"""Themis's reply schema in each provider's dialect, and replies read back."""

import json
from collections.abc import Callable
from pathlib import Path
from typing import Any, cast

import pytest

from themis_intelligence.evaluation.bundle import job_of, load_bundles
from themis_intelligence.investigation.schemas import reply_schema
from themis_intelligence.platform.adapters.schema_translation import (
    ANTHROPIC,
    OPENAI_STRICT,
    Dialect,
    translate,
    within_limits,
)

Schema = dict[str, Any]


def test_entries_that_dont_make_a_map_are_left_for_validation_to_refuse() -> None:
    schema = {
        "type": "object",
        "properties": {"params": {"type": "object", "additionalProperties": {"type": "string"}}},
    }
    twice = [{"key": "pod", "value": "a"}, {"key": "pod", "value": "b"}]
    got = translate(schema, ANTHROPIC)
    assert json.loads(got.decode(json.dumps({"params": twice})))["params"] == twice


def test_a_choice_too_big_for_the_limits_is_asked_for_as_text() -> None:
    def fix(name: str) -> Schema:
        props = {f"{name}{i}": {"type": "integer"} for i in range(13)}
        return {"type": "object", "properties": props, "additionalProperties": False}

    schema = {
        "type": "object",
        "properties": {"params": {"anyOf": [fix("a"), fix("b")]}},
        "required": ["params"],
    }
    assert not within_limits(schema, ANTHROPIC)
    got = translate(schema, ANTHROPIC)
    assert got.sent["properties"]["params"]["type"] == "string"
    assert within_limits(got.sent, ANTHROPIC)
    assert json.loads(got.decode(json.dumps({"params": '{"a1": 3}'}))) == {"params": {"a1": 3}}


def nested(node: Schema) -> list[Schema]:
    return [
        *cast(Schema, node.get("properties", {})).values(),
        *cast(Schema, node.get("$defs", {})).values(),
        *node.get("anyOf", []),
        *node.get("allOf", []),
        *([node["items"]] if "items" in node else []),
    ]


ANTHROPIC_WORDS = {
    "type", "enum", "const", "anyOf", "allOf", "$ref", "$defs", "description", "title",
    "default", "required", "properties", "additionalProperties", "items", "minItems", "format",
}  # fmt: skip


def anthropic_accepts(node: Schema) -> bool:
    if not set(node) <= ANTHROPIC_WORDS or node.get("minItems", 0) > 1:
        return False
    if node.get("type") == "object" and node.get("additionalProperties") is not False:
        return False
    return all(anthropic_accepts(n) for n in nested(node))


# OpenAI's strict mode: https://developers.openai.com/api/docs/guides/structured-outputs
OPENAI_WORDS = {
    "type", "enum", "const", "anyOf", "$ref", "$defs", "description", "title", "required",
    "properties", "additionalProperties", "items", "minItems", "maxItems", "minimum", "maximum",
    "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "pattern", "format",
}  # fmt: skip


def openai_accepts(node: Schema) -> bool:
    if not set(node) <= OPENAI_WORDS or ("$ref" in node and len(node) > 1):
        return False
    if node.get("type") == "object" and (
        node.get("additionalProperties") is not False
        or set(node.get("required", [])) != set(node.get("properties", {}))
    ):
        return False
    return all(openai_accepts(n) for n in nested(node))


@pytest.mark.parametrize(
    ("dialect", "accepts"),
    [(ANTHROPIC, anthropic_accepts), (OPENAI_STRICT, openai_accepts)],
    ids=["anthropic", "openai-strict"],
)
def test_every_cases_reply_schema_is_one_the_provider_accepts(
    dialect: Dialect, accepts: Callable[[Schema], bool]
) -> None:
    for b in load_bundles(Path(__file__).parents[3] / "scenarios"):
        sent = translate(reply_schema(job_of(b)), dialect).sent
        assert sent["type"] == "object", b.scenario
        assert accepts(sent) and within_limits(sent, dialect), b.scenario
