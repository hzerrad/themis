# Themis

Themis is a governance engine for AI-proposed fixes to Kubernetes incidents and workloads. Fixes come from Themis's own LLM investigator or from any AI SRE agent. Themis applies a fix only if the evidence backs it, policy allows it and a person approves it, then checks that it worked.

Models propose; deterministic controls decide. A Kubernetes write is never considered a success because the API accepted it: Themis watches the result and reports whether the incident was resolved, contained, unchanged, worsened, or impossible to judge.

> **Status:** early development (v0.1). Five kinds of incident on Deployments are covered: a bad rollout, pods that never become ready, a memory limit set too low, probe restart storms and CPU throttling. Three fixes: rolling back, changing memory and CPU requests and limits, and changing probe timings. Not for production use yet.

## What it does

1. **Alertmanager fires** (for example `KubePodCrashLooping`). Themis resolves the alert to the workload it is about, deduplicates it into an incident, and records why it ignores the alerts it ignores.
2. **Evidence and facts.** Themis reads the target through named, read-only templates (rollout history, pod states, events, log tails, and memory and CPU use from the Kubernetes metrics API or Prometheus), hides secrets, and checks facts in code, such as *the new revision runs out of memory* or *the failures started after the rollout*. Each answer is yes, no or can't tell, and points to the evidence behind it. A check never trusts the text of an event alone: the pods' own status has to agree.
3. **Investigation.** A model reads that evidence and may ask for more: any OpenAI-compatible endpoint, such as Ollama or vLLM, or Claude through Anthropic's API. It proposes a fix that the incident's scenario packs offer, citing evidence. An outside AI SRE agent can suggest a fix through the same API instead. Every suggestion is untrusted: one for another target, an unknown fix, or made-up evidence is rejected and recorded as a safety event. Evidence containing text that looks like instructions or forged tool output is flagged, to the investigator and to the person approving.
4. **Governance.** Themis applies a fix only if a check that justifies it is true for the incident. It derives the action from the live cluster rather than from the proposal, checks ownership (a GitOps-managed target only ever gets a recommendation, never a change behind Argo CD's or Flux's back), evaluates your CEL policy, and binds everything that matters into a digest. If the checks support a fix the investigator didn't propose, a person is told.
5. **Approval.** In the UI you compare the proposal with the action derived from the cluster, see exactly what changes, and approve the digest you were shown, or deny it.
6. **Execution.** A separate executor, the only component with write access, rechecks the digest and the live target, dry-runs the change, then writes it. If anything changed since approval, it refuses.
7. **Verification.** Themis watches the workload for a window and decides the outcome. A change made by someone else during the window makes the result `inconclusive`, never a false success.

While an incident waits, Themis also watches for changes made outside it. A fix made by hand or through GitOps closes the incident as resolved; a change that does not help closes it and opens a new one for the target's new state. Every escalation can be posted to a webhook.

## Try it

To try Themis in about 10 minutes, on kind or on your own cluster, follow the **[quickstart](docs/quickstart.md)**: install it, send it alerts, and approve a rollback.

For a closer look at each step, see the bad-rollout demo on a local [kind](https://kind.sigs.k8s.io/) cluster, with real Prometheus and Alertmanager: **[docs/demo.md](docs/demo.md)**.

To install Themis in a cluster, see the **[Helm chart](deploy/helm/themis/README.md)**. Themis brings no model: point it at any OpenAI-compatible endpoint, hosted or local, or at Claude. Or run it without one (`model.enabled=false`): it still collects evidence and runs its checks, and each incident waits for a fix suggested by a person or an AI SRE agent, governed exactly like its own.

## Principles

- **Read-only by default.** The component that can change workloads is not installed unless you enable it.
- **Models never authorize.** Model output is an untrusted proposal. Authorization is a deterministic decision over live cluster state and explicit policy, then a human approval bound to a digest.
- **Evidence before assertion.** Diagnoses cite the facts that support and contradict them, and `unknown` is a real answer that blocks action. Free text from the cluster, such as logs, can raise a question but never justify a fix on its own.
- **Verification is mandatory.** Every action is followed by a measured outcome.
- **Bring your own model.** Works with hosted providers or local, air-gapped endpoints such as Ollama or vLLM.
- **Measured, not claimed.** A replayable evaluation suite, including prompt-injection attacks, runs on every change; CI fails if a harmful fix would reach a person. See the [results across models](docs/evaluation.md) and [how it works](eval/README.md).

## Architecture

| Component | Language | Role |
|---|---|---|
| `themis-core` | Go | Intake, evidence, checks, governance, approvals, verification, reconciliation, audit, HTTP API and UI |
| `themis-intelligence` | Python | The bounded investigation loop and model access; holds no cluster or database credentials |
| `themis-executor` | Go | Executes approved, digest-bound actions after independent re-verification; installed only when enabled |
| PostgreSQL | | Durable state, the audit trail, and the job queue |

More in [docs/architecture.md](docs/architecture.md). Configuration, including profiles and policy, is described in [docs/configuration.md](docs/configuration.md).
Logs, metrics and traces, and how to follow one incident across the components, are in [docs/observability.md](docs/observability.md).

## Supported versions

Themis supports the same Kubernetes releases the Kubernetes project maintains: the three most recent minor versions. The window moves with each Kubernetes release.

| Dependency | Supported |
|---|---|
| Kubernetes | 1.35, 1.36, 1.37 |
| Alertmanager | any release sending webhook payload version `4` |
| Model | any OpenAI-compatible chat completions endpoint with JSON-schema structured output, or Claude through Anthropic's API |

CI runs the tests against every Kubernetes version in the window, and weekly against the next, unreleased one. Themis logs the warnings the API server sends, such as for a deprecated API, and counts them in the `themis_kube_api_deprecation_warnings_total` metric.

Versions outside the window are not tested and may not work. The Helm chart refuses to install on an older Kubernetes, and Themis rejects Alertmanager webhooks of an unknown payload version.

## Development

Requirements: Docker, plus Go, [uv](https://docs.astral.sh/uv/), Helm and kubectl. Their versions are pinned in `mise.toml`: with [mise](https://mise.jdx.dev/), `mise install` installs them. Other tools (buf, golangci-lint, kind, envtest) are pinned in `tools/go.mod` and run through `go tool`, so no separate install is needed.

```bash
make help      # list targets
make lint      # lint Go, Python, protobuf
make test      # unit and integration tests (needs Docker for PostgreSQL)
make e2e       # end-to-end scenarios on a fresh kind cluster, with a fake model
make scenario-check  # every scenario pack's cases, replayed against recorded model replies
make vuln      # known vulnerabilities in the Go and Python dependencies
```

To run the components on your machine against kind while you work on them, see [Running locally](docs/demo.md#running-locally).

See [CONTRIBUTING.md](CONTRIBUTING.md) for conventions. To teach Themis a new kind of incident, add a scenario pack: see [docs/scenarios.md](docs/scenarios.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
