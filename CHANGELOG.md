# Changelog

## v0.1.0

The first release: the bad-rollout incident class, end to end.

- Alertmanager intake with a durable inbox: alerts resolved to their workload, deduplicated into incidents, and recorded with a reason when rejected.
- Read-only evidence templates (rollout history, pod states, events, container log tails), redacted before they are stored or sent.
- Deterministic three-valued predicates for rollouts, OOM kills, prior health and GitOps ownership.
- A bounded investigation loop over any OpenAI-compatible model, with typed decisions, token budgets, leases and heartbeats.
- Governance that derives digest-bound actions from live state and decides them with CEL policy, failing closed.
- Digest-bound, expiring, single-use approvals in the UI, with every change an action makes shown before approval.
- An executor that rechecks the digest and the live target, dry-runs, and writes with an optimistic-concurrency precondition.
- Verification over a window that judges outcomes and refuses to credit a fix someone else made.
- Reconciliation of incidents whose target changed outside Themis, and webhook notices for every escalation.
- A Helm chart, read-only by default, and an end-to-end test on kind.
