"""HolmesGPT's answer becomes a suggestion only when it chooses a fix the
incident offers, for the incident's own workload, with parameters that fit
that fix's schema."""

import json
from typing import Any

import pytest

from themis.v1 import common_pb2
from themis_intelligence.holmesgpt.decide import NoSuggestion, Suggestion, decide, offered

TARGET = common_pb2.TargetRef(
    kind="Deployment", namespace="shop", name="checkout", uid="u1", group="apps"
)
EVIDENCE = {"ev-history", "ev-pods"}

ROLLBACK = "deployment.rollback-to-revision"
PARTIAL = "deployment.roll-back-changes"
RESOURCES = "deployment.set-resources"
PROBES = "deployment.set-probes"


def _probe(*fields: str) -> dict[str, Any]:
    return {
        "type": "object",
        "additionalProperties": False,
        "minProperties": 1,
        "properties": {f: {"type": "integer", "minimum": 1, "maximum": 2147483647} for f in fields},
    }


def _quantities() -> dict[str, Any]:
    q = {"type": "string"}
    return {
        "type": "object",
        "additionalProperties": False,
        "minProperties": 1,
        "properties": {"memory": q, "cpu": q},
    }


# The parameter schemas of the fixes Themis ships, as GetIncident sends them.
SCHEMAS: dict[str, dict[str, Any]] = {
    ROLLBACK: {
        "type": "object",
        "additionalProperties": False,
        "required": ["revision"],
        "properties": {"revision": {"type": "integer", "minimum": 1}},
    },
    PARTIAL: {
        "type": "object",
        "additionalProperties": False,
        "required": ["revision", "changes"],
        "properties": {
            "revision": {"type": "integer", "minimum": 1},
            "changes": {
                "type": "array",
                "minItems": 1,
                "maxItems": 20,
                "items": {"type": "string", "minLength": 1, "maxLength": 300},
            },
        },
    },
    RESOURCES: {
        "type": "object",
        "additionalProperties": False,
        "required": ["containers"],
        "properties": {
            "containers": {
                "type": "object",
                "minProperties": 1,
                "additionalProperties": {
                    "type": "object",
                    "additionalProperties": False,
                    "minProperties": 1,
                    "properties": {"requests": _quantities(), "limits": _quantities()},
                },
            }
        },
    },
    PROBES: {
        "type": "object",
        "additionalProperties": False,
        "required": ["container"],
        "minProperties": 2,
        "properties": {
            "container": {"type": "string", "minLength": 1},
            "liveness": _probe("timeoutSeconds", "periodSeconds", "failureThreshold"),
            "readiness": _probe("timeoutSeconds", "periodSeconds", "successThreshold"),
        },
    },
}
CAPABILITIES = [
    common_pb2.Capability(name=n, description=f"{n} does it.", params_schema=json.dumps(s))
    for n, s in SCHEMAS.items()
]
FIXES = offered(CAPABILITIES)


def answer(fix: str = ROLLBACK, params: Any = None, **over: Any) -> str:
    given = {"revision": 1} if params is None else params
    base: dict[str, Any] = {
        "fix": fix,
        "params": given if isinstance(given, str) else json.dumps(given),
        "namespace": "shop",
        "workload": "checkout",
        "rationale": "Revision 2 is OOMKilled; revision 1 ran fine.",
        "expected_effect": "checkout runs revision 1 again and its pods stay up.",
        "evidence": ["ev-pods", "made-up", "ev-pods"],
    }
    return json.dumps(base | over)


@pytest.mark.parametrize(
    ("fix", "params"),
    [
        (ROLLBACK, {"revision": 1}),
        (PARTIAL, {"revision": 1, "changes": ["the environment variable MODE of app changed"]}),
        (RESOURCES, {"containers": {"app": {"limits": {"memory": "512Mi"}}}}),
        (PROBES, {"container": "app", "liveness": {"timeoutSeconds": 3}}),
    ],
)
def test_takes_each_fix_on_offer(fix: str, params: dict[str, Any]) -> None:
    got = decide(answer(fix, params), TARGET, FIXES, EVIDENCE)
    assert got == Suggestion(
        fix=fix,
        params=params,
        rationale="Revision 2 is OOMKilled; revision 1 ran fine.",
        expected_effect="checkout runs revision 1 again and its pods stay up.",
        cited=("ev-pods",),
    )


def test_takes_parameters_given_as_an_object() -> None:
    analysis = json.dumps(json.loads(answer()) | {"params": {"revision": 1}})
    got = decide(analysis, TARGET, FIXES, EVIDENCE)
    assert isinstance(got, Suggestion) and got.params == {"revision": 1}


@pytest.mark.parametrize(
    ("analysis", "why"),
    [
        ("The pod runs out of memory; roll back to revision 1.", "format"),
        (json.dumps({"fix": ROLLBACK}), "format"),
        (answer(extra="field"), "format"),
        (answer("none", {}), "chose none"),
        (answer(workload="payments"), "another workload"),
        (answer(namespace="kube-system"), "another workload"),
        (answer("deployment.scale", {"replicas": 3}), "doesn't offer"),
        (answer(params="revision 1"), "aren't JSON"),
        (answer(params="[1]"), "aren't a JSON object"),
        (answer(params={"revision": "1"}), "don't fit its schema: '1' is not of type 'integer'"),
        (answer(params={"revision": 0}), "don't fit its schema"),
        (answer(params={"revision": 1, "force": True}), "don't fit its schema"),
        (answer(PARTIAL, {"revision": 1, "changes": []}), "don't fit its schema"),
        (answer(RESOURCES, {"containers": {}}), "don't fit its schema"),
        (answer(PROBES, {"container": "app"}), "don't fit its schema"),
        (answer(rationale="  "), "no reason"),
        (answer(evidence=["made-up"]), "cited no evidence"),
    ],
)
def test_suggests_nothing(analysis: str, why: str) -> None:
    got = decide(analysis, TARGET, FIXES, EVIDENCE)
    assert isinstance(got, NoSuggestion) and why in got.reason


def test_only_what_the_incident_offers_and_the_adapter_can_check() -> None:
    # A fix Themis has, but not on offer for this incident, as when the
    # chart's holmesgpt.fixes leaves it out.
    only_rollback = offered(CAPABILITIES[:1])
    got = decide(answer(RESOURCES, {"containers": {"app": {}}}), TARGET, only_rollback, EVIDENCE)
    assert isinstance(got, NoSuggestion) and "doesn't offer" in got.reason
    unreadable = offered([common_pb2.Capability(name=ROLLBACK, params_schema="not json")])
    got = decide(answer(), TARGET, unreadable, EVIDENCE)
    assert isinstance(got, NoSuggestion) and "can't be checked" in got.reason
