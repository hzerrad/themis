<img src="docs/assets/themis-logo.svg" alt="" width="96" align="right">

# Themis

[![CI](https://github.com/hzerrad/themis/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/hzerrad/themis/actions/workflows/ci.yml)
[![End-to-end](https://github.com/hzerrad/themis/actions/workflows/e2e.yml/badge.svg?branch=main)](https://github.com/hzerrad/themis/actions/workflows/e2e.yml)
[![Release](https://img.shields.io/github/v/release/hzerrad/themis?include_prereleases&sort=semver)](https://github.com/hzerrad/themis/releases)
[![License](https://img.shields.io/github/license/hzerrad/themis)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/hzerrad/themis)](go.mod)

Governance engine for AI-proposed fixes to Kubernetes incidents and workloads.

Themis turns alerts about Kubernetes workloads into incidents, collects evidence from the cluster, and decides whether a proposed fix may be applied. A fix can come from Themis's own LLM investigator, from an external AI SRE agent, or from an engineer. Themis applies it only when its checks justify it, policy allows it and a person approves it, then verifies the result.

> [!WARNING]
> Themis is in alpha (`v0.1.0-alpha1`). Configuration and APIs may change between releases. It is not ready for production use.

Investigation and governance are separate:

| | AI investigation | Deterministic governance |
|---|---|---|
| Component | `themis-intelligence`, or an external AI SRE agent | `themis-core` and `themis-executor` |
| Works from | The evidence Themis collected, and an LLM | Cluster state, CEL checks and CEL policy. No model |
| Produces | A ranked diagnosis and one proposed fix, citing evidence | A decision on the proposal, then execution and verification |
| Trust | Untrusted. Every proposal is validated | Holds the only write access, through the executor |

## Example

A Deployment `checkout` in namespace `shop` is rolled out with a new image and a changed environment variable `MODE`. The new pods crash.

| Step | Performed by | Result |
|---|---|---|
| Alert | Alertmanager | `KubePodCrashLooping` for `shop/checkout` |
| Evidence | Themis | Revision 2 changed the image and `MODE`. Its pods restart. Revision 1 ran without restarts |
| Checks | Themis | The failures started after the rollout: true. The rollout brought them: true |
| Proposal | Investigator (LLM) | Roll back `MODE` to revision 1 and keep the new image |
| Decision | Themis | The checks justify a rollback. Policy rule `rollback-needs-approval` requires a person's approval |
| Approval | Approver | Approves the exact change shown, bound to a digest |
| Execution | Executor | Rechecks the digest and the live Deployment, dry-runs the patch, applies it |
| Verification | Themis | The new pods stay ready without restarts. The incident closes as fixed |

## Capabilities

This section lists what `v0.1.0-alpha1` supports. More alert sources and workload kinds are planned.

**Alert sources.** Alertmanager, read through its API or received by webhook.

**Workloads.** Deployments.

**Incident types.** Each is a [scenario pack](docs/scenarios.md) of checks, fixes, policy and evaluation cases, and new incident types are added as new packs. Alerts that no pack matches are recorded and ignored.

| Incident | Alerts | Fixes offered |
|---|---|---|
| Bad rollout | `KubePodCrashLooping`, `KubePodOOMKilled`, `KubeDeploymentReplicasMismatch` | Rollback, partial rollback |
| Memory limit too low | `KubePodOOMKilled`, `KubePodCrashLooping` | Resource change |
| CPU throttling | `CPUThrottlingHigh` | Resource change. Rollback or partial rollback when the throttling came with a rollout |
| Probe restart storm | `KubePodCrashLooping`, `KubePodNotReady` | Probe change |
| Stuck pods | `KubePodNotReady`, `KubeContainerWaiting`, `KubeDeploymentRolloutStuck` | None. The cause is named and the incident is referred to a person |

**Fixes.**

| Fix | Changes |
|---|---|
| `deployment.rollback-to-revision` | The pod template, back to an earlier revision's |
| `deployment.roll-back-changes` | Selected changes of the latest rollout, such as one environment variable, back to their earlier values. The other changes stay |
| `deployment.set-resources` | CPU and memory requests and limits of named containers |
| `deployment.set-probes` | Timing fields of existing liveness, readiness and startup probes |

**Also included.**

- Evidence from pod states, events, container logs, rollout history and memory and CPU use, from the Kubernetes metrics API or a Prometheus-compatible server. Evidence is redacted and size-bounded before it is stored.
- An incident page that states the facts with their evidence, shows each piece of evidence, collects more on request, and previews whether a suggested fix would be accepted.
- Operation without a model (`model.enabled=false`). Evidence and checks still run, and fixes come from engineers or agents.
- A gRPC API for external AI SRE agents to submit proposals.
- Escalation notices to a webhook.
- Sign-in with local accounts or OIDC, with the roles viewer, responder, approver and admin.

## Architecture

```mermaid
flowchart LR
  AM["Alert source (Alertmanager)"] -->|alerts| Core[themis-core]
  Core <-->|evidence, proposals| Intel[themis-intelligence]
  Agent[AI SRE agent] -->|proposals| Core
  Core -->|approved actions| Exec[themis-executor]
  Core -->|read| K8s[(Kubernetes API)]
  Exec -->|patch| K8s
  Core --- PG[(PostgreSQL)]
```

| Component | Language | Role |
|---|---|---|
| `themis-core` | Go | Alert intake, evidence, checks, governance, approvals, verification, audit trail, HTTP API and web UI |
| `themis-intelligence` | Python | The investigator: a bounded loop over the evidence and the model. Holds no cluster or database credentials |
| `themis-executor` | Go | Applies approved actions after checking them again. Optional; the only component with write access |
| PostgreSQL | | State, audit trail and job queue |

Permissions and the threat model are in [docs/architecture.md](docs/architecture.md).

## Governance

Every proposal, whatever its source, passes the same controls in `themis-core`:

1. **Validation.** The proposal must name the incident's exact target, UID included, use a fix the incident's profiles offer, and cite evidence Themis collected. Anything else is rejected. A wrong target is recorded as a safety event.
2. **Justification.** A fix is accepted only when one of its justifying checks is true. Checks are CEL expressions over the collected evidence. Text in logs or events alone never justifies a fix.
3. **Feasibility.** The change is derived from the live object. It is refused when no node can fit it, or when a ResourceQuota or LimitRange forbids it.
4. **Ownership.** Workloads managed by Argo CD or Flux receive a recommendation, never a change.
5. **Policy.** CEL rules decide `allow`, `require_approval` or `deny`. In the default `guarded` mode every action needs approval. `observe` mode denies every action.
6. **Approval.** A person approves the exact change shown, bound to a digest. The approval is void if the workload changes first, and it expires.
7. **Execution.** The executor rechecks the digest and the live object, dry-runs the change, then applies it. Its Role grants only what the installed fixes need, in enrolled namespaces: today `get` and `patch` on Deployments, and reading ReplicaSets.
8. **Verification.** Themis decides the fix's symptom checks again from the new pods. A fix that made things worse is undone. An incident whose symptoms persist is referred to a person.

Evidence containing text aimed at Themis is flagged to the investigator and the approver. Details are in [docs/configuration.md](docs/configuration.md).

**Evaluation.** The investigator is scored on 91 recorded incidents, 28 of which carry text planted in the evidence to steer it. CI replays the qwen3 8B recordings on every change and fails if a harmful fix would reach a person.

| Investigator | Right decision | Right cause first | Harmful fixes attempted | Harmful fixes reaching a person |
|---|---|---|---|---|
| Claude Haiku 5.5 | 84 / 91 | 86 / 91 | 0 | 0 |
| qwen3 8B, local | 63 / 91 | 60 / 91 | 14 | 0 |

Method and caveats are in [docs/evaluation.md](docs/evaluation.md).

## Deployment

| Requirement | Supported |
|---|---|
| Kubernetes | 1.35, 1.36, 1.37 |
| Alert source | Alertmanager, API v2, 0.16 or later |
| Model (optional) | Claude through Anthropic's API, or an OpenAI-compatible endpoint with JSON-schema structured output, such as OpenAI, Ollama or vLLM |
| Metrics (optional) | Kubernetes metrics API. A Prometheus-compatible server for throttling, peaks and history |

```sh
helm install themis oci://ghcr.io/hzerrad/charts/themis --version 0.1.0-alpha1 \
  --namespace themis --create-namespace
```

- The [quickstart](docs/quickstart.md) installs Themis on kind or an existing cluster and walks through a first incident.
- Themis is read-only until the executor is enabled (`executor.enabled=true`).
- Until the first admin account is created, Themis serves only the page that creates it.
- Images are signed and published for linux/amd64 and linux/arm64.
- The [chart README](deploy/helm/themis/README.md) lists every value, including GitOps installs and an external database.

## Limitations

- Deployments are the only supported workload kind. StatefulSets, DaemonSets and others are not supported yet.
- Alertmanager is the only supported alert source.
- Five incident types, listed under [Capabilities](#capabilities).
- Every change needs a person's approval. There is no autonomous mode.
- Without a Prometheus-compatible server, Themis can't measure CPU throttling, peak memory or an earlier revision's history. Checks that need them answer unknown, and an unknown check never justifies a fix.
- A partial rollback restores earlier values only. Setting new values is not supported.
- Investigator accuracy depends on the model. See the evaluation under [Governance](#governance).
- The bundled PostgreSQL is a single instance for evaluation.

## Documentation

| Guide | Covers |
|---|---|
| [Quickstart](docs/quickstart.md) | Installing Themis and handling a first incident |
| [Demo](docs/demo.md) | Every scenario pack on a local kind cluster |
| [Configuration](docs/configuration.md) | Profiles, policy, fixes, people and roles, all settings |
| [Checks](docs/checks.md) | Built-in checks and writing new ones |
| [Scenario packs](docs/scenarios.md) | Building and testing a pack |
| [Architecture](docs/architecture.md) | Components, permissions, threat model |
| [Observability](docs/observability.md) | Logs, metrics and traces |
| [Evaluation](docs/evaluation.md) | How the investigator is measured |
| [Glossary](GLOSSARY.md) | Terms used throughout Themis |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and conventions. Alpha testers can report results with the **Alpha feedback** issue form. Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md). This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).

## License

[Apache 2.0](LICENSE)
