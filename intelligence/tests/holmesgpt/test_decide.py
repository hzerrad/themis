"""HolmesGPT's answer becomes a suggestion only when every check holds."""

import json
from typing import Any

import pytest

from themis.v1 import common_pb2
from themis_intelligence.holmesgpt.decide import (
    NoSuggestion,
    Revision,
    Suggestion,
    decide,
    revisions,
)

TARGET = common_pb2.TargetRef(
    kind="Deployment", namespace="shop", name="checkout", uid="u1", group="apps"
)
HISTORY = [Revision(1, False, ("shop:1",)), Revision(2, True, ("shop:2",))]
EVIDENCE = {"ev-history", "ev-pods"}


def answer(**over: Any) -> str:
    base: dict[str, Any] = {
        "decision": "rollback",
        "namespace": "shop",
        "deployment": "checkout",
        "revision": 1,
        "rationale": "Revision 2 is OOMKilled; revision 1 ran fine.",
        "evidence": ["ev-pods", "made-up"],
    }
    return json.dumps(base | over)


def test_a_good_answer() -> None:
    got = decide(answer(), TARGET, "ev-history", HISTORY, EVIDENCE)
    assert got == Suggestion(1, "Revision 2 is OOMKilled; revision 1 ran fine.", ("ev-pods",))


def test_cites_the_history_when_nothing_cited_is_real() -> None:
    got = decide(answer(evidence=["made-up"]), TARGET, "ev-history", HISTORY, EVIDENCE)
    assert isinstance(got, Suggestion) and got.cited == ("ev-history",)


@pytest.mark.parametrize(
    ("analysis", "why"),
    [
        ("The pod runs out of memory; roll back to revision 1.", "format"),
        (json.dumps({"decision": "rollback"}), "format"),
        (answer(extra="field"), "format"),
        (answer(revision="1"), "format"),
        (answer(decision="none", revision=None), "no rollback"),
        (answer(deployment="payments"), "another workload"),
        (answer(namespace="kube-system"), "another workload"),
        (answer(revision=2), "isn't an earlier one"),
        (answer(revision=7), "isn't an earlier one"),
        (answer(revision=None), "isn't an earlier one"),
        (answer(rationale="  "), "no reason"),
    ],
)
def test_suggests_nothing(analysis: str, why: str) -> None:
    got = decide(analysis, TARGET, "ev-history", HISTORY, EVIDENCE)
    assert isinstance(got, NoSuggestion) and why in got.reason


def test_only_deployments() -> None:
    sts = common_pb2.TargetRef(kind="StatefulSet", namespace="shop", name="checkout")
    got = decide(answer(), sts, "ev-history", HISTORY, EVIDENCE)
    assert isinstance(got, NoSuggestion) and "Deployment" in got.reason


def test_reads_the_rollout_history() -> None:
    data = {
        "deployment": {"name": "checkout"},
        "revisions": [
            {
                "replicaSet": "a",
                "revision": "1",
                "containers": [{"name": "app", "image": "shop:1"}],
            },
            {
                "replicaSet": "b",
                "revision": "2",
                "current": True,
                "containers": [{"name": "app", "image": "shop:2"}],
            },
        ],
    }
    items = [
        common_pb2.EvidenceItem(id="ev-pods", template="pod_states", data=b"{}"),
        common_pb2.EvidenceItem(
            id="ev-history", template="rollout_history", data=json.dumps(data).encode()
        ),
    ]
    assert revisions(items) == ("ev-history", HISTORY)
    assert revisions(items[:1]) == (None, [])
    broken = [common_pb2.EvidenceItem(id="x", template="rollout_history", data=b"not json")]
    assert revisions(broken) == ("x", [])
