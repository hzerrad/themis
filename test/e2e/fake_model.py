"""A deterministic OpenAI-compatible model for the end-to-end test.

It answers as a careful investigator would: it reads the logs first, then
proposes rolling the target back to its prior revision, citing the evidence
it was given and copying the target exactly. Governance, policy, execution
and verification still run for real; only the model is scripted.
"""

import json
import re
from http.server import BaseHTTPRequestHandler, HTTPServer

TARGET = re.compile(r"Target \(use this exact object as proposal\.target\): (\{.*?\})\n")
EVIDENCE = re.compile(r'<evidence id="([^"]+)" template="([^"]+)"[^>]*>\n(.*?)\n</evidence>', re.S)


def decide(messages: list[dict[str, str]]) -> dict[str, object]:
    text = "\n".join(m["content"] for m in messages)
    if "The proposal was rejected" in messages[-1]["content"]:
        return {"kind": "conclude_inconclusive", "summary": "the proposal was rejected"}
    evidence = EVIDENCE.findall(text)
    templates = {template for _, template, _ in evidence}
    if "container_logs_tail" in text and "container_logs_tail" not in templates:
        return {"kind": "request_evidence", "template": "container_logs_tail", "params": {}}

    target = json.loads(TARGET.search(text).group(1))
    history = next(json.loads(body) for _, template, body in evidence if template == "rollout_history")
    current = int(history["deployment"]["currentRevision"])
    prior = max(int(r["revision"]) for r in history["revisions"] if r["revision"] and int(r["revision"]) < current)
    cited = [eid for eid, template, _ in evidence if template in ("rollout_history", "pod_states")]
    hypothesis = {
        "root_cause_class": "bad_rollout",
        "summary": f"revision {current} is OOMKilled; revision {prior} was healthy",
        "supporting": cited,
        "contradicting": [],
        "missing": [],
        "status": "supported",
    }
    return {
        "kind": "propose",
        "summary": f"Roll back to revision {prior}",
        "hypotheses": [hypothesis],
        "proposal": {
            "capability": "deployment.rollback-to-revision",
            "target": target,
            "revision": prior,
            "expected_effect": "pods stop being OOMKilled",
            "rationale": f"revision {current} is OOMKilled since its rollout; revision {prior} served without failures",
            "cited_evidence_ids": cited,
            "cited_predicates": ["oom_killed_new_revision", "failure_onset_after_rollout", "prior_revision_was_healthy"],
        },
    }


class Handler(BaseHTTPRequestHandler):
    def do_POST(self) -> None:
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        decision = decide(body["messages"])
        reply = {
            "model": body.get("model", "fake"),
            "choices": [{"message": {"role": "assistant", "content": json.dumps(decision)}}],
            "usage": {"prompt_tokens": 1000, "completion_tokens": 100},
        }
        data = json.dumps(reply).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
        print("decided", decision["kind"], flush=True)


if __name__ == "__main__":
    HTTPServer(("", 8080), Handler).serve_forever()
