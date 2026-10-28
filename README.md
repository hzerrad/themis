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

## Try it locally

The bad-rollout scenario on kind, with a local model through [Ollama](https://ollama.com). Every process listens on `127.0.0.1` only.

```bash
make kind-up dev-db                # cluster and PostgreSQL
export DATABASE_URL=postgres://themis:themis@127.0.0.1:5432/themis?sslmode=disable
ollama pull qwen3:8b               # with `ollama serve` running
make run                           # themis-core, in one terminal
make run-executor                  # themis-executor, in another
make run-intelligence              # the investigator, in a third
make demo-up demo-break            # deploy checkout, then roll out an OOMKilled revision
make demo-alert                    # once a checkout pod is crash looping
```

Open <http://127.0.0.1:8080> and sign in with the token in `bin/dev/admin-token`. The incident shows the evidence, the predicates, the investigator's hypotheses, and the proposed rollback next to the action derived from the cluster. Approving sends the digest you were shown; the executor checks it again before writing, and verification decides whether the incident is resolved.

To install in a cluster instead, see `deploy/helm/themis`. The chart is read-only unless `executor.enabled=true`.

## License

Apache License 2.0. See [LICENSE](LICENSE).
