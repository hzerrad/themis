# Themis

Themis investigates Kubernetes workload incidents with AI and, when policy allows, applies a constrained remediation whose effect is measured, reversed if it made things worse, and fully audited.

Models propose; deterministic controls decide. A Kubernetes write is never considered successful because the API accepted it: Themis observes the result and reports whether the incident was resolved, contained, unchanged, worsened, or impossible to judge.

> **Status:** early development. Nothing here is ready for production use.

## Principles

- **Read-only by default.** The component that can change workloads is not installed unless you enable it.
- **Models never authorize.** Model output is an untrusted proposal. Authorization is a deterministic decision over live cluster state and explicit policy.
- **Evidence before assertion.** Diagnoses cite the facts that support and contradict them, and `unknown` is a real answer.
- **Verification is mandatory.** Every action is followed by a measured outcome.
- **Self-hosted, bring your own model.** Works with hosted model providers or local, air-gapped endpoints.

## Architecture

| Component | Language | Role |
|---|---|---|
| `themis-core` | Go | Kubernetes operator: intake, evidence, predicates, governance, approvals, verification, audit, API and UI |
| `themis-intelligence` | Python | Bounded investigation loop and model access; holds no cluster or database credentials |
| `themis-executor` | Go | Executes approved, digest-bound actions after independent re-verification; installed only in write mode |
| PostgreSQL | | Durable state, audit trail, and job queue |

## Supported versions

Themis supports the same Kubernetes releases the Kubernetes project maintains: the three most recent minor versions. The window moves with each Kubernetes release.

| Dependency | Supported |
|---|---|
| Kubernetes | 1.35, 1.36, 1.37 |
| Alertmanager | any release sending webhook payload version `4` |

Versions outside the window are not tested and may not work. The Helm chart refuses to install on an older Kubernetes, and Themis rejects Alertmanager webhooks of an unknown payload version.

## Development

Requirements: Go, [uv](https://docs.astral.sh/uv/), Node.js, Docker, and Helm. Other tools (buf, golangci-lint, kind) are pinned in `tools/go.mod` and run through `go tool`, so no separate install is needed.

```bash
make help      # list targets
make lint      # lint Go, Python, and protobuf
make test      # run unit tests
make kind-up   # start a local multi-node cluster
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for conventions.

## License

Apache License 2.0. See [LICENSE](LICENSE).
