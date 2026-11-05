"""What the model may answer at each step of an investigation."""

import json
from collections.abc import Iterator
from dataclasses import dataclass
from typing import Annotated, Any, Literal, cast

from annotated_types import GroupedMetadata, MaxLen
from pydantic import BaseModel, BeforeValidator, Field

from themis.v1 import investigation_pb2

SCHEMA_VERSION = "6"

# Every list and string in a reply is bounded, so a model that repeats itself
# is stopped by the schema, which strict providers enforce as they generate,
# instead of running into the output limit and losing the whole reply.
MAX_HYPOTHESES = 3

# A reply that runs past a bound is rejected, and the model asked again, unless
# the part is only read by people: text such as a summary is cut to its bound,
# and a list of notes keeps its first items. Names, the fix, its parameters and
# whatever a reply cites are never cut, since cutting them could change what
# Themis does.


def _trimmed(text: str, limit: int) -> str:
    """text, cut to at most limit characters at a word, with an ellipsis."""
    if len(text) <= limit:
        return text
    cut = text[: limit - 1]
    # Drop the word the cut runs through, unless it is the only one.
    broken = not cut[-1].isspace() and not text[limit - 1].isspace()
    if broken and len(words := cut.rsplit(maxsplit=1)) == 2:
        cut = words[0]
    return cut.rstrip(" \t\n,;:.") + "…"


@dataclass(frozen=True)
class Prose(GroupedMetadata):
    """Text for people, of at most limit characters: longer text is trimmed."""

    limit: int

    def __iter__(self) -> Iterator[object]:
        # Listed after the bound, the trimming runs before the bound is checked.
        yield MaxLen(self.limit)
        yield BeforeValidator(lambda v: _trimmed(v, self.limit) if isinstance(v, str) else v)


@dataclass(frozen=True)
class FirstOf(GroupedMetadata):
    """A list of notes for people, of at most limit items: the rest are dropped."""

    limit: int

    def __iter__(self) -> Iterator[object]:
        yield MaxLen(self.limit)
        yield BeforeValidator(
            lambda v: cast(list[Any], v)[: self.limit] if isinstance(v, list) else v
        )


# A name: of a Kubernetes object, a root cause, a check, a fix or evidence.
Name = Annotated[str, Field(max_length=253)]
# What a hypothesis is missing, in a few words.
Note = Annotated[str, Prose(160)]


class TargetOut(BaseModel):
    """The incident's target, copied exactly."""

    group: Name
    kind: Name
    namespace: Name
    name: Name
    uid: Name


class HypothesisOut(BaseModel):
    # One of the root causes the core sent: reply_schema lists them.
    root_cause_class: Name
    summary: Annotated[str, Prose(300)]
    supporting: list[Name] = Field(
        max_length=6, description="IDs of evidence supporting the hypothesis."
    )
    contradicting: list[Name] = Field(max_length=6, description="IDs of evidence against it.")
    missing: Annotated[list[Note], FirstOf(5)] = Field(
        description="Evidence that would settle it but is missing."
    )
    status: Literal["supported", "refuted", "open"]


class ProposalOut(BaseModel):
    capability: Name = Field(description="One of the allowed fixes, by name.")
    target: TargetOut
    params: dict[str, Any] = Field(description="The fix's parameters, as its schema describes.")
    expected_effect: Annotated[str, Prose(300)]
    rationale: Annotated[str, Prose(600)]
    cited_evidence_ids: list[Name] = Field(max_length=8)
    # Checks the core sent, by name: reply_schema lists them.
    cited_predicates: list[Name] = Field(max_length=8)


class StepDecision(BaseModel):
    kind: Literal["request_evidence", "propose", "conclude_inconclusive"]
    template: Name | None = Field(
        default=None, description="For request_evidence: a template name."
    )
    params: dict[str, Name] = Field(default_factory=dict[str, str])
    summary: Annotated[str, Prose(400)] = ""
    hypotheses: list[HypothesisOut] = Field(
        default_factory=list[HypothesisOut], max_length=MAX_HYPOTHESES
    )
    proposal: ProposalOut | None = Field(default=None, description="For propose.")


def checks(job: investigation_pb2.Investigation) -> list[str]:
    """The checks a proposal may cite: those the core ran for the incident."""
    return list(dict.fromkeys(p.name for p in job.predicates))


def root_causes(job: investigation_pb2.Investigation) -> list[str]:
    """The root causes a hypothesis may name: those of the incident's profile."""
    return [c.name for c in job.root_causes]


def reply_schema(job: investigation_pb2.Investigation) -> dict[str, Any]:
    """The step decision's JSON Schema, narrowed to what the core sent: the
    fixes on offer and their parameters, the checks it ran and the root causes
    of the incident's profile, so the model is guided to valid ones. The loop
    still refuses any other, and the core checks every suggestion again."""
    schema = StepDecision.model_json_schema()
    hypothesis = schema["$defs"]["HypothesisOut"]["properties"]
    proposal = schema["$defs"]["ProposalOut"]["properties"]
    if causes := root_causes(job):
        hypothesis["root_cause_class"]["enum"] = causes
    if cited := checks(job):
        proposal["cited_predicates"]["items"]["enum"] = cited
    names = [c.name for c in job.capabilities]
    if not names and not job.candidate_capabilities:
        # No fix is on offer, so proposing isn't a choice the model has.
        kind = schema["properties"]["kind"]
        kind["enum"] = [k for k in kind["enum"] if k != "propose"]
    if names:
        proposal["capability"] = {
            "type": "string",
            "enum": names,
            "description": "One of the allowed fixes, by name.",
        }
    params = [json.loads(c.params_schema) for c in job.capabilities if c.params_schema]
    if len(params) == 1:
        proposal["params"] = params[0]
    elif params:
        proposal["params"] = {"anyOf": params}
    return schema
