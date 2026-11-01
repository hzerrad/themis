# Architecture

Themis is three processes and a database. They are separated by what each may do, not by feature.

```
 Alertmanager ──webhook──▶ themis-core ◀──gRPC── themis-intelligence ──▶ your model
                             │    ▲                (no cluster or DB access)
                  reads      │    │ HTTP API, UI
                             ▼    │
                         Kubernetes   ◀──patch── themis-executor
                             ▲                    (only writer; off by default)
                             │
                         PostgreSQL: incidents, evidence, actions, audit, jobs
```

| Component | May | May not |
|---|---|---|
| `themis-core` | Read enrolled workloads, pods, logs and events; run governance, approvals, verification; serve the API and UI | Read Secrets in workload namespaces; write to workloads |
| `themis-intelligence` | Call the model; ask the core for evidence by template name; submit proposals | Reach the cluster or the database: it has no service account token, and a NetworkPolicy allows only the core and the model |
| `themis-executor` | Get and patch Deployments in enrolled namespaces | Anything else; it acts only on an action ID, and rechecks everything itself |

Each API is behind its own bearer token: Alertmanager's, the investigator's, the executor's, and the admin token for the API and UI. The tokens are files, read on every request, so rotation needs no restart.

## An incident's life

```
received → investigating → diagnosed → proposed → awaiting_approval → authorized → executing → verifying → resolved
    │            │             │           │              │                                      │
 rejected   escalated    closed_no_action  recommended   escalated (expired)        escalated / contained_escalated
            canceled     self_resolved     closed_no_action  closed_no_action (denied)      restoring → escalated
```

When a fix is applied and the workload is unchanged or worse at the end of the verification window, Themis puts it back as it was before it acted (`restoring`), checks that Kubernetes rolled that version out, and hands the incident to a person (`escalated`). People then investigate the workload as they left it, not a mix of the incident and Themis's attempt. Putting it back is an action of its own: a rollback to the revision the fix replaced, with every check a fix has, decided by policy. It needs no approval unless a rule matching `action.restore == "true"` asks for one; then, or when Kubernetes no longer keeps that revision, Themis leaves its change in place and escalates at once. A fix that partly helped (`contained_escalated`) is kept, and an outcome changed by someone else during the window (`inconclusive`) is never put back.

Every transition is stored with its time, reason and actor, together with the job it starts, in one transaction: a crash never leaves a state change without its follow-up, or the reverse. Jobs run at least once, and every job is safe to repeat.

## What protects what

| Risk | Guard |
|---|---|
| A model told by a log line to act on another workload | The core accepts proposals only for the incident's exact target (UID included), only for capabilities the profile allows, and only citing evidence it collected. Anything else is stored as rejected, and a wrong target is recorded as a safety event. |
| A proposal that misdescribes the change | The action is derived from the live cluster, never from the proposal's text. The UI shows both side by side, with every field the action changes. |
| An approval reused, or for something that changed | An approval is bound to the action's digest, expires, and is consumed by the execution claim. The digest covers the target's UID, generation, a hash of its pod template, its field manager, and the exact template being restored, but not `resourceVersion`, which changes on every status update. |
| The cluster changing between approval and write | The executor rereads the target and recomputes the digest; any difference makes the action `stale` and nothing is written. The write uses the fresh `resourceVersion` as a precondition, after a server-side dry run. |
| A write whose outcome is unknown | A timeout after the request was sent is recorded as `ambiguous` and escalated, never retried blindly. |
| Declaring success that is not one | Verification watches the outcome for a window. A change by someone else during it makes the outcome `inconclusive`. |
| Fighting GitOps | Ownership is checked live, from Argo CD and Flux markers and field managers, and the default policy denies changing a managed target. |
| Acting on a stale picture | Every 30 seconds Themis compares open incidents' targets with the template they were investigated against. A fix made elsewhere closes the incident; a change that does not help supersedes it with a new one. |
| Secrets reaching the model or the UI | Evidence never includes pod specs, and every item is redacted. The UI shows what an action changes, hiding values that look secret by name or content. |
| Evidence posing as instructions | Evidence reaches the model fenced as untrusted data, with any tag-like text escaped. |
| A misconfiguration | The configuration is validated whole, policy rules are compiled at startup, and a rule that fails at runtime denies. |

## Data

PostgreSQL holds incidents and their transitions, alert states, rejected alerts, evidence (redacted, size-bounded), predicate results, investigations, proposals, safety events, actions, approvals, executions and verifications. Jobs run on [River](https://riverqueue.com), in the same database, so a state change and its job commit together.

## Contracts

The core and the investigator speak gRPC, defined in [`api/proto/themis/v1`](../api/proto/themis/v1). The core never connects to the investigator: the investigator leases work, renews its lease while it works, and reports back. A result reported on a lost lease is refused.
