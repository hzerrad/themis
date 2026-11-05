"""Themis's reply schema in the subset of JSON Schema that Anthropic's structured
output accepts, and the reply turned back into Themis's shape.

The subset has no maps keyed by name, such as a fix's containers or an evidence
template's params, and no free-form values: a part like that is asked for as a
string holding its JSON, with its schema in the description, and parsed back
from the reply. Constraints the subset lacks, such as maxLength or minimum, move
into the description, so the model still reads them. Themis validates every
reply against its own schema either way.

A request may hold at most 24 optional properties and 16 that are unions. When
the schema has more, which a choice between several fixes can bring, each such
choice is asked for as JSON text too.
"""

import json
from dataclasses import dataclass
from typing import Any, cast

# Anthropic's limits on one request's schemas.
MAX_OPTIONAL = 24
MAX_UNIONS = 16

FORMATS = {
    "date-time",
    "time",
    "date",
    "duration",
    "email",
    "hostname",
    "uri",
    "ipv4",
    "ipv6",
    "uuid",
}

# Marks a part sent as JSON text. It never reaches the provider.
_AS_TEXT = "x-themis-json-text"

Schema = dict[str, Any]


@dataclass(frozen=True)
class ReplySchema:
    """A reply schema as Anthropic gets it, and how to read its replies."""

    sent: Schema
    _marked: Schema

    def decode(self, text: str) -> str:
        """The reply in Themis's shape. A reply that isn't JSON is left as it
        is, for Themis's validation to report."""
        try:
            value = json.loads(text)
        except ValueError:
            return text
        defs = cast(dict[str, Schema], self._marked.get("$defs", {}))
        return json.dumps(_decode(value, self._marked, defs))


def reply_schema(schema: Schema) -> ReplySchema:
    marked = _convert(schema, unions_as_text=False)
    if not within_limits(marked):
        marked = _convert(schema, unions_as_text=True)
    return ReplySchema(sent=_unmarked(marked), _marked=marked)


def _as_text(node: Schema) -> Schema:
    shape = {k: v for k, v in node.items() if k not in ("description", "title", "default")}
    note = f"JSON, written as a string, matching this schema: {json.dumps(shape)}"
    described = node.get("description")
    return {
        "type": "string",
        "description": f"{described}\n\n{note}" if described else note,
        _AS_TEXT: True,
    }


def _is_open(node: Schema) -> bool:
    """An object whose keys aren't listed, or a value of any type."""
    if "$ref" in node or any(k in node for k in ("anyOf", "oneOf", "allOf")):
        return False
    t = node.get("type")
    if t is None:
        return True
    if t == "object" or (isinstance(t, list) and "object" in t):
        return "properties" not in node or node.get("additionalProperties", False) is not False
    return False


def _is_choice_of_objects(node: Schema) -> bool:
    branches = cast(list[Schema], node.get("anyOf") or node.get("oneOf") or [])
    objects = [b for b in branches if b.get("type") == "object" or "$ref" in b]
    return len(objects) > 1


def _convert(node: Schema, unions_as_text: bool) -> Schema:
    if _is_open(node) or (unions_as_text and _is_choice_of_objects(node)):
        return _as_text(node)
    rest = dict(node)
    out: Schema = {}
    if "$defs" in rest:
        defs = cast(dict[str, Schema], rest.pop("$defs"))
        out["$defs"] = {k: _convert(v, unions_as_text) for k, v in defs.items()}
    if "$ref" in rest:
        # The subset takes nothing beside a reference.
        out["$ref"] = rest.pop("$ref")
        return out
    for word in ("anyOf", "oneOf", "allOf"):
        if word in rest:
            branches = cast(list[Schema], rest.pop(word))
            out["allOf" if word == "allOf" else "anyOf"] = [
                _convert(b, unions_as_text) for b in branches
            ]
    for word in ("type", "enum", "const", "description", "title", "default", "required"):
        if word in rest:
            out[word] = rest.pop(word)
    if "properties" in rest:
        props = cast(dict[str, Schema], rest.pop("properties"))
        out["properties"] = {k: _convert(v, unions_as_text) for k, v in props.items()}
        rest.pop("additionalProperties", None)
        out["additionalProperties"] = False
    if "items" in rest:
        out["items"] = _convert(cast(Schema, rest.pop("items")), unions_as_text)
    if rest.get("minItems") in (0, 1):
        out["minItems"] = rest.pop("minItems")
    if rest.get("format") in FORMATS:
        out["format"] = rest.pop("format")
    if rest:
        # Constraints the subset lacks: the model reads them, Themis checks them.
        note = json.dumps(rest, sort_keys=True)
        described = out.get("description")
        out["description"] = f"{described}\n\n{note}" if described else note
    return out


def _counts(node: Any) -> tuple[int, int]:
    """Optional properties and union-typed properties, everywhere in node."""
    optional = unions = 0
    if isinstance(node, dict):
        d = cast(Schema, node)
        props = cast(dict[str, Schema], d.get("properties", {}))
        optional += len(props) - len(cast(list[str], d.get("required", [])))
        unions += sum(1 for p in props.values() if "anyOf" in p or isinstance(p.get("type"), list))
        for v in d.values():
            o, u = _counts(v)
            optional, unions = optional + o, unions + u
    elif isinstance(node, list):
        for v in cast(list[Any], node):
            o, u = _counts(v)
            optional, unions = optional + o, unions + u
    return optional, unions


def within_limits(schema: Schema) -> bool:
    optional, unions = _counts(schema)
    return optional <= MAX_OPTIONAL and unions <= MAX_UNIONS


def _unmarked(node: Any) -> Any:
    if isinstance(node, dict):
        return {k: _unmarked(v) for k, v in cast(Schema, node).items() if k != _AS_TEXT}
    if isinstance(node, list):
        return [_unmarked(v) for v in cast(list[Any], node)]
    return node


def _resolve(node: Schema, defs: dict[str, Schema]) -> Schema:
    ref = node.get("$ref")
    if isinstance(ref, str) and ref.startswith("#/$defs/"):
        return defs.get(ref.removeprefix("#/$defs/"), {})
    return node


def _fits(value: Any, node: Schema, defs: dict[str, Schema]) -> bool:
    node = _resolve(node, defs)
    if node.get(_AS_TEXT):
        return isinstance(value, str)
    if "anyOf" in node:
        return any(_fits(value, b, defs) for b in cast(list[Schema], node["anyOf"]))
    match node.get("type"):
        case "object":
            props = cast(Schema, node.get("properties", {}))
            return isinstance(value, dict) and set(cast(Schema, value)) <= set(props)
        case "array":
            return isinstance(value, list)
        case "string":
            return isinstance(value, str)
        case "null":
            return value is None
        case "boolean":
            return isinstance(value, bool)
        case "integer" | "number":
            return isinstance(value, int | float) and not isinstance(value, bool)
        case _:
            return True


def _decode(value: Any, node: Schema, defs: dict[str, Schema]) -> Any:
    node = _resolve(node, defs)
    if node.get(_AS_TEXT):
        if not isinstance(value, str):
            return value
        try:
            return json.loads(value)
        except ValueError:
            return value
    if "anyOf" in node:
        for branch in cast(list[Schema], node["anyOf"]):
            if _fits(value, branch, defs):
                return _decode(value, branch, defs)
        return value
    if isinstance(value, dict) and "properties" in node:
        props = cast(dict[str, Schema], node["properties"])
        return {
            k: _decode(v, props[k], defs) if k in props else v
            for k, v in cast(Schema, value).items()
        }
    if isinstance(value, list) and "items" in node:
        items = cast(Schema, node["items"])
        return [_decode(v, items, defs) for v in cast(list[Any], value)]
    return cast(Any, value)
