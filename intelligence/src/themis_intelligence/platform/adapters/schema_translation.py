"""Themis's reply schema in the subset of JSON Schema a provider's structured
output accepts, and the reply turned back into Themis's shape.

Two providers need one: Anthropic always, and OpenAI in strict mode. What they
share is translated the same way. Neither subset has maps keyed by name, such
as a fix's containers or an evidence template's params: a map is asked for as a
list of entries, each a key and its value, so the value's schema still binds
the model, and turned back into a map. Nor free-form values: a part like that
is asked for as a string holding its JSON, with its schema in the description,
and parsed back from the reply. Models write JSON inside a string less
reliably, so only what can't be described otherwise is asked for that way.
Every object is closed. Constraints the subset lacks, such as maxLength, move
into the description, so the model still reads them. Themis validates every
reply against its own schema either way.

Where they differ, each Dialect says so: which constraints the provider checks
itself, whether every property must be required, and the limits on one
request's schema.
"""

import json
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any, cast

Schema = dict[str, Any]

# Mark a part sent as JSON text, a map sent as a list of entries, and a property
# made nullable only because the provider requires every one. None reaches the
# provider.
_AS_TEXT = "x-themis-json-text"
_AS_ENTRIES = "x-themis-map-entries"
_NULL_ADDED = "x-themis-null-added"
_MARKS = (_AS_TEXT, _AS_ENTRIES, _NULL_ADDED)

# Kept as they are, in every dialect, besides the structure (properties, items,
# $defs, $ref, anyOf and the like), which is translated.
_ANNOTATIONS = ("type", "enum", "const", "description", "title")


@dataclass(frozen=True)
class Dialect:
    """What a provider's structured output accepts, where providers differ."""

    # A constraint the provider checks itself, with its value; any other moves
    # into the description.
    checks: Callable[[str, Any], bool]
    # Defaults are kept, or dropped where every property is given anyway.
    keeps_default: bool
    # allOf is kept, or described like any other constraint the subset lacks.
    keeps_all_of: bool
    # Every property is required, an optional one as nullable; the null is
    # dropped from the reply, so Themis's own default applies.
    all_required: bool
    # The most optional and union-typed properties one request may hold; None
    # for no such limit.
    limits: tuple[int, int] | None


_ANTHROPIC_FORMATS = {
    "date-time", "time", "date", "duration", "email", "hostname", "uri", "ipv4", "ipv6", "uuid",
}  # fmt: skip
_OPENAI_FORMATS = _ANTHROPIC_FORMATS - {"uri"}
_OPENAI_BOUNDS = {
    "minItems", "maxItems", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum",
    "multipleOf", "pattern",
}  # fmt: skip

# Anthropic's subset: minItems up to 1, a few formats, and no more than 24
# optional properties and 16 unions in one request.
ANTHROPIC = Dialect(
    checks=lambda word, value: (
        (word == "minItems" and value in (0, 1))
        or (word == "format" and value in _ANTHROPIC_FORMATS)
    ),
    keeps_default=True,
    keeps_all_of=True,
    all_required=False,
    limits=(24, 16),
)

# OpenAI's strict mode: bounds on numbers and arrays, patterns and a few
# formats, but no string length, no allOf and no defaults; and every property
# required.
OPENAI_STRICT = Dialect(
    checks=lambda word, value: (
        word in _OPENAI_BOUNDS or (word == "format" and value in _OPENAI_FORMATS)
    ),
    keeps_default=False,
    keeps_all_of=False,
    all_required=True,
    limits=None,
)


@dataclass(frozen=True)
class ReplySchema:
    """A reply schema as the provider gets it, and how to read its replies."""

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


@dataclass(frozen=True)
class _Asking:
    """Which parts are asked for as JSON text, besides free-form values."""

    # Only maps whose entries count toward the limits.
    maps_as_text: bool = False
    unions_as_text: bool = False


# From the most a subset can describe to the least, the first within limits.
# When a schema has more optional or union-typed properties than Anthropic
# allows, maps whose entries count toward them are asked for as JSON text; and
# when it still has more, which a choice between several fixes can bring, each
# such choice too.
_FALLBACKS = (_Asking(), _Asking(maps_as_text=True), _Asking(True, True))


def translate(schema: Schema, dialect: Dialect) -> ReplySchema:
    marked = _Translation(dialect, _FALLBACKS[0]).convert(schema)
    for asking in _FALLBACKS[1:]:
        if within_limits(marked, dialect):
            break
        marked = _Translation(dialect, asking).convert(schema)
    return ReplySchema(sent=_unmarked(marked), _marked=marked)


def _noted(node: Schema, note: str) -> str:
    described = node.get("description")
    return f"{described}\n\n{note}" if described else note


def _as_text(node: Schema) -> Schema:
    shape = {k: v for k, v in node.items() if k not in ("description", "title", "default")}
    note = f"JSON, written as a string, matching this schema: {json.dumps(shape)}"
    return {"type": "string", "description": _noted(node, note), _AS_TEXT: True}


def _map_values(node: Schema) -> Schema | None:
    """The schema of a map's values, if node is a map whose values have one."""
    values = node.get("additionalProperties")
    if node.get("type") != "object" or "properties" in node or not isinstance(values, dict):
        return None
    return cast(Schema, values) or None


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


def _allows_null(node: Schema) -> bool:
    t = node.get("type")
    if t == "null" or (isinstance(t, list) and "null" in t):
        return True
    return any(_allows_null(b) for b in cast(list[Schema], node.get("anyOf", [])))


def _nullable(node: Schema) -> Schema:
    """node, or null, which the reply drops."""
    if _allows_null(node):
        return node
    if "anyOf" in node:
        return {**node, "anyOf": [*node["anyOf"], {"type": "null"}], _NULL_ADDED: True}
    outer = {k: node[k] for k in ("description", "title") if k in node}
    inner = {k: v for k, v in node.items() if k not in outer}
    return {"anyOf": [inner, {"type": "null"}], **outer, _NULL_ADDED: True}


@dataclass(frozen=True)
class _Translation:
    dialect: Dialect
    asking: _Asking

    def convert(self, node: Schema) -> Schema:
        values = _map_values(node)
        if values is not None and not (self.asking.maps_as_text and _counts(values) != (0, 0)):
            return self._as_entries(node, values)
        if _is_open(node) or (self.asking.unions_as_text and _is_choice_of_objects(node)):
            return _as_text(node)
        rest = dict(node)
        out: Schema = {}
        if "$defs" in rest:
            defs = cast(dict[str, Schema], rest.pop("$defs"))
            out["$defs"] = {k: self.convert(v) for k, v in defs.items()}
        if "$ref" in rest:
            # Neither subset takes anything beside a reference.
            out["$ref"] = rest.pop("$ref")
            return out
        for word in ("anyOf", "oneOf"):
            if word in rest:
                out["anyOf"] = [self.convert(b) for b in cast(list[Schema], rest.pop(word))]
        if "allOf" in rest and self.dialect.keeps_all_of:
            out["allOf"] = [self.convert(b) for b in cast(list[Schema], rest.pop("allOf"))]
        for word in (*_ANNOTATIONS, "default", "required"):
            if word in rest:
                out[word] = rest.pop(word)
        if not self.dialect.keeps_default:
            out.pop("default", None)
        if "properties" in rest:
            props = cast(dict[str, Schema], rest.pop("properties"))
            required = cast(list[str], out.get("required", []))
            out["properties"] = {
                k: _nullable(self.convert(v))
                if self.dialect.all_required and k not in required
                else self.convert(v)
                for k, v in props.items()
            }
            if self.dialect.all_required:
                out["required"] = list(props)
            rest.pop("additionalProperties", None)
            out["additionalProperties"] = False
        if "items" in rest:
            out["items"] = self.convert(cast(Schema, rest.pop("items")))
        for word, value in list(rest.items()):
            if self.dialect.checks(word, value):
                out[word] = rest.pop(word)
        if rest:
            # Constraints the subset lacks: the model reads them, Themis checks them.
            out["description"] = _noted(out, json.dumps(rest, sort_keys=True))
        return out

    def _as_entries(self, node: Schema, values: Schema) -> Schema:
        entry = {
            "type": "object",
            "properties": {"key": {"type": "string"}, "value": self.convert(values)},
            "required": ["key", "value"],
            "additionalProperties": False,
        }
        rest = {
            k: v
            for k, v in node.items()
            if k not in ("type", "additionalProperties", "description", "title", "default")
        }
        note = "Entries by key, each key once."
        if rest:
            note += f" {json.dumps(rest, sort_keys=True)}"
        return {
            "type": "array",
            "items": entry,
            "description": _noted(node, note),
            _AS_ENTRIES: True,
        }


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


def within_limits(schema: Schema, dialect: Dialect) -> bool:
    if dialect.limits is None:
        return True
    optional, unions = _counts(schema)
    max_optional, max_unions = dialect.limits
    return optional <= max_optional and unions <= max_unions


def _unmarked(node: Any) -> Any:
    if isinstance(node, dict):
        return {k: _unmarked(v) for k, v in cast(Schema, node).items() if k not in _MARKS}
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
    if node.get(_AS_ENTRIES):
        return isinstance(value, list)
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
    if node.get(_AS_ENTRIES):
        return _decode_entries(value, node, defs)
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
            if not (v is None and k in props and props[k].get(_NULL_ADDED))
        }
    if isinstance(value, list) and "items" in node:
        items = cast(Schema, node["items"])
        return [_decode(v, items, defs) for v in cast(list[Any], value)]
    return cast(Any, value)


def _decode_entries(value: Any, node: Schema, defs: dict[str, Schema]) -> Any:
    """The entries as a map. Entries that don't make one, such as a key given
    twice, are left as they are, for Themis's validation to refuse."""
    if not isinstance(value, list):
        return value
    entries = cast(list[Any], value)
    if not all(isinstance(e, dict) and set(cast(Schema, e)) == {"key", "value"} for e in entries):
        return entries
    pairs = [(e["key"], e["value"]) for e in cast(list[Schema], entries)]
    keys = {k for k, _ in pairs if isinstance(k, str)}
    if len(keys) != len(pairs):
        return entries
    values = cast(Schema, node["items"])["properties"]["value"]
    return {k: _decode(v, values, defs) for k, v in pairs}
