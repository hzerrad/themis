"""A real scenario, and a model endpoint that follows a script."""

import json
from pathlib import Path
from typing import Any

import httpx

from themis_intelligence.evaluation.bundle import Bundle, load_bundles
from themis_intelligence.platform.contract import ProviderRequest, ProviderResponse, Usage

SCENARIOS = Path(__file__).parents[3] / "eval" / "scenarios"


def oom(**truth: Any) -> Bundle:
    b = load_bundles(SCENARIOS, ["rollout-oom"])[0]
    return b.model_copy(update={"truth": b.truth.model_copy(update=truth)})


TARGET = {
    "group": "apps",
    "kind": "Deployment",
    "namespace": "shop",
    "name": "checkout",
    "uid": "uid-deployment-checkout",
}
LOGS: dict[str, Any] = {"kind": "request_evidence", "template": "container_logs_tail", "params": {}}
HYPOTHESIS = {
    "root_cause_class": "memory_limit_regression",
    "summary": "revision 2 lowered the limit",
    "supporting": ["ev-pod_states"],
    "contradicting": [],
    "missing": [],
    "status": "supported",
}
DECLINE: dict[str, Any] = {
    "kind": "conclude_inconclusive",
    "summary": "can't tell",
    "hypotheses": [],
}


def propose(**overrides: Any) -> dict[str, Any]:
    return {
        "kind": "propose",
        "summary": "roll back",
        "hypotheses": [HYPOTHESIS],
        "proposal": {
            "capability": "deployment.rollback-to-revision",
            "target": TARGET,
            "revision": 1,
            "expected_effect": "pods stop being killed",
            "rationale": "revision 2 is killed",
            "cited_evidence_ids": ["ev-pod_states"],
            "cited_predicates": ["oom_killed_new_revision"],
            **overrides,
        },
    }


class ScriptedAdapter:
    """Replies with each step in turn, starting over at the end, so it serves many runs."""

    def __init__(self, *steps: Any, tokens: int = 100) -> None:
        self.steps = steps
        self.tokens = tokens
        self.calls = 0
        self.requests: list[ProviderRequest] = []

    async def send(self, req: ProviderRequest) -> ProviderResponse:
        step = self.steps[self.calls % len(self.steps)]
        self.calls += 1
        self.requests.append(req)
        if isinstance(step, Exception):
            raise step
        content = step if isinstance(step, str) else json.dumps(step)
        return ProviderResponse(
            content=content,
            usage=Usage(input_tokens=self.tokens, output_tokens=self.tokens),
            model="served-model",
        )


DOWN = httpx.ConnectError("connection refused")
