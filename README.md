<img src="docs/assets/themis-logo.svg" alt="" width="96" align="right">

# Themis

[![CI](https://github.com/hzerrad/themis/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/hzerrad/themis/actions/workflows/ci.yml)
[![End-to-end](https://github.com/hzerrad/themis/actions/workflows/e2e.yml/badge.svg?branch=main)](https://github.com/hzerrad/themis/actions/workflows/e2e.yml)
[![Release](https://img.shields.io/github/v/release/hzerrad/themis?include_prereleases&sort=semver)](https://github.com/hzerrad/themis/releases)
[![License](https://img.shields.io/github/license/hzerrad/themis)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/hzerrad/themis)](go.mod)

Governance engine for AI-proposed fixes to Kubernetes incidents and workloads.

When an alert fires on a Deployment, a fix can come from Themis's own LLM investigator, an AI SRE agent, or an engineer. Themis decides whether it runs. It reads the cluster itself, and a fix runs only when Themis's checks justify it, your policy allows it and a person approves the exact change. Afterwards, Themis measures whether the symptom went away.

> [!WARNING]
> `v0.1.0-alpha1` is the first release. Configuration and APIs will change. Try it on a test cluster, and don't run it in production yet.

## An example

A rollout of `checkout` ships a new image and changes the environment variable `MODE`. The new pods start crashing.

Themis reads the rollout history, pod states, events and logs. Its checks find that the crashes began with the rollout and that revision 1 ran without them. The investigator proposes putting `MODE` back and keeping the new image. Themis accepts the proposal, since the checks justify a rollback, and the incident page asks:

> **Waiting for your approval**
> Roll back checkout's environment variable MODE to revision 1?
> Keeps the rollout's new image.

The approver sees the exact change, the image digest that stays, and a warning when Helm or CI will undo the change on the next deploy. Once approved, a separate executor checks that nothing moved since, dry-runs the patch and applies it. Themis then watches the pods. If they stop crashing, the incident closes as fixed. If they keep crashing, it goes to a person, and if the change made things worse, Themis first puts `MODE` back as it was.

## What it handles

Five kinds of incident, each shipped as a [scenario pack](docs/scenarios.md): a directory of checks, fixes, policy and evaluation cases. A new kind of incident is a new pack.

| Incident | Alerts | What Themis can do |
|---|---|---|
| Bad rollout | `KubePodCrashLooping`, `KubePodOOMKilled`, `KubeDeploymentReplicasMismatch` | Roll back to the earlier revision, or undo only some of the rollout's changes |
| Memory limit too low | `KubePodOOMKilled`, `KubePodCrashLooping` | Raise memory limits and requests |
| CPU throttling | `CPUThrottlingHigh` | Raise CPU limits and requests, or roll back when the throttling came with a rollout |
| Probe restart storm | `KubePodCrashLooping`, `KubePodNotReady` | Change probe timings: delay, period, timeout and thresholds |
| Stuck pods | `KubePodNotReady`, `KubeContainerWaiting`, `KubeDeploymentRolloutStuck` | Name the cause, such as an image that can't be pulled or pods no node can take, and refer the incident to a person |

Themis chooses between a rollback and a raise from evidence. A rollback needs a rollout that changed what the pods run and a new version that behaves worse than the one before, by its own pods or by Prometheus history. When that can't be established, a person decides.

## How a fix is decided

1. **Evidence.** Themis collects it from the cluster: rollout history and what each revision changed, pod states, events, recent logs, and memory and CPU use from the metrics API or Prometheus. Values that look secret are redacted before anything is stored or shown to a model.
2. **Checks.** CEL expressions answer yes, no or unknown from that evidence alone. A fix needs a check that justifies it to answer yes. Text in a log or event never justifies a fix on its own. The pods' status has to agree.
3. **The proposal.** Treated as untrusted. A fix for another workload, a fix the pack doesn't offer, or a citation of evidence that doesn't exist is refused and recorded. Evidence holding text aimed at Themis is flagged to the approver.
4. **Feasibility and policy.** Themis derives the change from the live object, refuses one the cluster can't run (no node fits it, a ResourceQuota or LimitRange forbids it), and evaluates your CEL policy. Workloads managed by Argo CD or Flux get a recommendation only.
5. **Approval.** A person approves the change they were shown, bound to a digest. If the workload changes first, the approval is void.
6. **Execution.** The executor holds the only write access and is installed only when you enable it. It rechecks the digest and the live object, dry-runs the change, then applies it.
7. **Verification.** An accepted API write proves nothing. Themis watches for the symptom the fix was meant to cure. A fix that made things worse is undone. A change someone else makes in the meantime makes the result inconclusive.

Without an investigator (`model.enabled=false`), steps 1 and 2 still run. The incident page shows the facts with their evidence, and an engineer or an AI SRE agent suggests the fix, which goes through the same steps.

## Measured

The investigator is scored on 91 recorded incidents across the five packs. Of those, 28 carry text planted in logs, events or config to steer it.

| Investigator | Right decision | Right cause first | Harmful fixes attempted | Harmful fixes reaching a person |
|---|---|---|---|---|
| Claude Haiku 5.5 | 84 / 91 | 86 / 91 | 0 | 0 |
| qwen3 8B, on a local GPU | 63 / 91 | 60 / 91 | 14 | 0 |

The small model attempted 14 harmful fixes and Themis refused all of them. CI replays its recorded replies on every change and fails if a harmful fix would reach a person. Method and caveats are in [docs/evaluation.md](docs/evaluation.md).

## Install

The [quickstart](docs/quickstart.md) takes about 10 minutes on kind or on your own cluster.

```sh
helm install themis oci://ghcr.io/hzerrad/charts/themis --version 0.1.0-alpha1 \
  --namespace themis --create-namespace
```

- **Model.** Claude through Anthropic's API, any OpenAI-compatible endpoint (OpenAI, Ollama, vLLM), or none.
- **Alerts.** Themis reads active alerts from Alertmanager's API, at an address the chart finds at install. Alertmanager can push to a webhook instead.
- **Access.** Nothing works until the first admin account exists. People sign in with local accounts or through OIDC, as viewer, responder, approver or admin.
- **Images.** Signed, for amd64 and arm64.

The [chart README](deploy/helm/themis/README.md) covers every value, and GitOps installs with Argo CD or Flux.

## Architecture

| Component | Language | Role |
|---|---|---|
| `themis-core` | Go | Alert intake, evidence, checks, governance, approvals, verification, audit trail, HTTP API and web UI |
| `themis-intelligence` | Python | The investigator: a bounded loop over the evidence and the model. Holds no cluster or database credentials |
| `themis-executor` | Go | Applies approved changes after checking them again. The only component that writes to workloads |
| PostgreSQL | | State, audit trail and job queue |

See [docs/architecture.md](docs/architecture.md) for the threat model and the permissions each component holds.

## Supported versions

| Dependency | Supported |
|---|---|
| Kubernetes | 1.35, 1.36, 1.37, the three minor versions the Kubernetes project maintains |
| Alertmanager | API v2, 0.16 or later. Webhook payload version `4` in push mode |
| Model | OpenAI-compatible chat completions with JSON-schema structured output, or Claude through Anthropic's API |

CI tests every Kubernetes version in that window, and weekly the next, unreleased one. The chart refuses to install on an older Kubernetes.

## Documentation

| Guide | Covers |
|---|---|
| [Quickstart](docs/quickstart.md) | Install, enroll a namespace, approve a first fix |
| [Demo](docs/demo.md) | Every pack on a local kind cluster with Prometheus and Alertmanager |
| [Configuration](docs/configuration.md) | Profiles, policy, people and roles, every setting |
| [Checks](docs/checks.md) | The built-in checks and how to write one |
| [Scenario packs](docs/scenarios.md) | How a pack is built and tested |
| [Architecture](docs/architecture.md) | Components, permissions, threat model |
| [Observability](docs/observability.md) | Logs, metrics, traces, and following one incident across components |
| [Evaluation](docs/evaluation.md) | How the investigator is measured, and the results |
| [Glossary](GLOSSARY.md) | The terms Themis uses |

## Development

You need Docker, Go, [uv](https://docs.astral.sh/uv/), Node, Helm and kubectl, at the versions pinned in `mise.toml`. `mise install` installs them. The other tools run through `go tool`, pinned in `tools/go.mod`.

```sh
make lint            # Go, Python, protobuf
make test            # unit and integration tests; PostgreSQL runs in Docker
make test-ui         # every page of the web UI, as each role, in jsdom
make e2e             # end-to-end scenarios on a fresh kind cluster
make scenario-check  # every pack's cases, replayed against recorded model replies
```

`make help` lists the rest. To run the components on your machine against kind, see [Running locally](docs/demo.md#running-locally).

## Contributing

Issues, scenario packs and fixes are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) first. If you tried the alpha, the **Alpha feedback** issue form asks what Themis decided and whether it was right.

Report vulnerabilities privately, as [SECURITY.md](SECURITY.md) describes. This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).

## License

[Apache 2.0](LICENSE)
