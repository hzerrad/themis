# Architecture

Themis is three processes and a database. They are separated by what each may do, not by feature.

```
 Alertmanager ◀──reads─── themis-core ◀──gRPC── themis-intelligence ──▶ your model
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
| `themis-core` | Read the active alerts from Alertmanager's API; read enrolled workloads, pods, logs and events, and their memory and CPU use from the metrics API or Prometheus; list nodes, and ResourceQuotas and LimitRanges in enrolled namespaces, to tell whether a resource change can run; run governance, approvals, verification; serve the API and UI | Read Secrets in workload namespaces; write to workloads |
| `themis-intelligence` | Call the model; ask the core for evidence by template name; submit proposals | Reach the cluster or the database: it has no service account token, and a NetworkPolicy allows only the core and the model |
| `themis-executor` | Get and patch Deployments in enrolled namespaces | Anything else; it acts only on an action ID, and rechecks everything itself |

Each API is behind its own bearer token: the alert webhook's, the investigator's, the executor's, and the admin token for the API and UI. The tokens are files, read on every request, so rotation needs no restart. People can also sign in to the UI through an OIDC provider, with a role each, so an approval records who gave it; see [People and roles](configuration.md#people-and-roles).

## An incident's life

```
received → investigating → diagnosed → proposed → awaiting_approval → authorized → executing → verifying → resolved
    │            │             │           │              │                                      │
 rejected   escalated    closed_no_action  recommended   escalated (expired)        escalated / contained_escalated
            canceled     self_resolved     closed_no_action  awaiting_suggestion (denied)   restoring → escalated
                                                         closed_no_action (denied and closed)
```

A denied fix leaves the incident `awaiting_suggestion`, waiting for another: one a person or an agent suggests, or one found by investigating it again. The new investigation is told which fixes people denied, and their reasons, as text for the model to weigh, never instructions, and it draws on the incident's token budget. A fix a person or an agent suggests that the checks or policy refuse leaves the incident waiting too, as does any refused fix, or an investigation that finds none, once a person has denied one or had the incident investigated again. Themis only recommending a fix, because GitOps owns the workload, still closes it.

When a fix is applied and the workload is unchanged or worse at the end of the verification window, Themis puts it back as it was before it acted (`restoring`), checks that Kubernetes rolled that version out, and hands the incident to a person (`escalated`). People then investigate the workload as they left it, not a mix of the incident and Themis's attempt. Putting it back is an action of its own: a rollback to the revision the fix replaced, with every check a fix has, decided by policy. It needs no approval unless a rule matching `action.restore == "true"` asks for one; then, or when Kubernetes no longer keeps that revision, Themis leaves its change in place and escalates at once. If someone changed the pod template after the fix, Themis leaves their change alone and escalates. If the restore can't be carried out, it escalates with its change still in place, so an incident is never left waiting. A fix that partly helped (`contained_escalated`) is kept, and an outcome changed by someone else during the window (`inconclusive`) is never put back.

Every transition is stored with its time, reason and actor, together with the job it starts, in one transaction: a crash never leaves a state change without its follow-up, or the reverse. Jobs run at least once, and every job is safe to repeat.

## What protects what

| Risk | Guard |
|---|---|
| A model told by a log line to act on another workload | The core accepts proposals only for the incident's exact target (UID included), only for capabilities one of its matching profiles offers, decided by those profiles' policies, and only citing evidence it collected. Anything else is stored as rejected, and a wrong target is recorded as a safety event. |
| A proposal that misdescribes the change | The action is derived from the live cluster, never from the proposal's text. The UI shows both side by side, with every field the action changes. |
| An approval reused, or for something that changed | An approval is bound to the action's digest, expires, and is consumed by the execution claim. The digest covers the target's UID, generation, a hash of its pod template, its field manager, and the exact template being restored, but not `resourceVersion`, which changes on every status update. |
| The cluster changing between approval and write | The executor rereads the target and recomputes the digest; any difference makes the action `stale` and nothing is written. The write uses the fresh `resourceVersion` as a precondition, after a server-side dry run. |
| A write whose outcome is unknown | A timeout after the request was sent is recorded as `ambiguous` and escalated, never retried blindly. |
| Declaring success that is not one | Verification watches the outcome for a window. A change by someone else during it makes the outcome `inconclusive`. |
| Fighting GitOps | Ownership is checked from the evidence and again live, from Argo CD and Flux markers and field managers. Before any policy, governance refuses to change a target that is managed, or may be, and only recommends the fix. |
| Acting on a stale picture | Every 30 seconds Themis compares open incidents' targets with the template they were investigated against. A fix made elsewhere closes the incident; a change that does not help supersedes it with a new one. |
| Secrets reaching the model or the UI | Evidence never includes pod specs, and every item is redacted. The UI shows what an action changes, hiding values that look secret by name or content. |
| Evidence posing as instructions | Evidence reaches the model fenced as untrusted data, with any tag-like text escaped. Text that looks like instructions, forged tool output or an order to make a fix is flagged in the core: the investigator, whichever it is, is told before it reads it, the approver is warned on the fix, and policy can read `evidence_has_instructions`. See [suspicious evidence](configuration.md#suspicious-evidence). |
| Evidence that talks a fix in or out | Logs, event messages and config values may raise a hypothesis, never settle a decision. A fix is refused unless a check its profile names to justify it is true, and when the checks support a fix the investigator didn't propose, a person is told. What operators read is built from checks and the fix's changes; the investigator's own summary is shown apart, as its note. See [justified fixes](configuration.md#justified-fixes). |
| A misconfiguration | The configuration is validated whole, policy rules are compiled at startup, and a rule that fails at runtime denies. |

## Data

PostgreSQL holds incidents and their transitions, alert states, rejected alerts, evidence (redacted, size-bounded), predicate results, investigations, proposals, safety events, actions, approvals, executions and verifications. Jobs run on [River](https://riverqueue.com), in the same database, so a state change and its job commit together.

## Contracts

The core and the investigator speak gRPC, defined in [`api/proto/themis/v1`](../api/proto/themis/v1). The core never connects to the investigator: the investigator leases work, renews its lease while it works, and reports back. A result reported on a lost lease is refused.

## Where the code lives

The repository follows the usual Go layout: binaries in `cmd/`, everything else in `internal/`, contracts in `api/`. Inside `internal/`, folders follow the picture above: what this install can do, the incident's life, how the outside talks to Themis, its configuration and its database, and the plumbing every part shares.

```
cmd/            themis-core, themis-executor
internal/
  core/         assembles themis-core
  executor/     themis-executor
  catalog/      what Themis can change, decide and read
  incident/     an incident's life, stage by stage
  workload/     the target as Themis reads it
  digest/       the action digest
  server/       the HTTP and gRPC APIs, and who is calling
  config/       configuration: the file, custom resources, setup
  store/        PostgreSQL
  platform/     plumbing: logs, metrics and traces, tokens, redaction
  testutil/     test helpers
scenarios/      the scenario packs
eval/           the packs' evaluation: bundles, conformance, scaffold
```

| Folder | What it holds |
|---|---|
| `cmd/themis-core`, `cmd/themis-executor` | The two Go binaries: flags, logging, then `internal/core` or `internal/executor` |
| `api/proto`, `gen/` | The gRPC contracts, and the code generated from them |
| `intelligence/` | The investigator, in Python |
| `internal/core` | Assembles themis-core from the packages below, and its probes |
| `internal/executor` | themis-executor: the only code that writes to workloads |
| `internal/catalog` | Everything this install can read, check and change, built once and handed out in parts: it loads the scenario packs, and says whether a profile asks for anything it doesn't have |
| `internal/catalog/capability` | **What Themis can change.** The interface every fix implements, and the registry |
| `internal/catalog/capability/rollback` | `deployment.rollback-to-revision` |
| `internal/catalog/capability/probes` | `deployment.set-probes`: the timing of a container's existing liveness, readiness and startup probes |
| `internal/catalog/capability/resources` | `deployment.set-resources`: containers' memory and CPU requests and limits, checked against the namespace's LimitRanges and ResourceQuotas and the nodes' allocatable |
| `internal/catalog/check` | **What Themis decides from evidence**, such as "the crash started with the rollout": checks written in YAML with CEL expressions, compiled at start-up, and the variables they see. The checks themselves are in the scenario packs; see [checks.md](checks.md) |
| `internal/catalog/evidence` | **What Themis can read** about a target: pod states, events, logs, rollout history, memory and CPU use |
| `internal/catalog/evidence/metrics` | How much memory and CPU a workload's containers use, asked the same way of any metrics source: the Kubernetes metrics API, or a server answering Prometheus queries. A new source is one more adapter here; nothing that reads evidence changes |
| `internal/catalog/evidence/hint` | One line per piece of missing evidence: what is missing, and the change that lets Themis read it |
| `scenarios/` | **The incidents Themis handles**, one scenario pack per folder: its profile, its checks and the evaluation cases that prove it, built into the binary; see [scenarios.md](scenarios.md) |
| `internal/incident` | An incident and its states |
| `internal/incident/intake` | Alerts in, incidents out (`alertmanager/`, and `target/` to find the workload an alert is about) |
| `internal/incident/investigation` | An incident's investigation: prepares it, hands it to an investigator under a lease, reads the evidence it asks for within its profile and budget, records what it couldn't have, and ends it with the state its outcome leads to; waits for a suggested fix when there is none |
| `internal/incident/suggestion` | Takes a fix suggested for an incident, from the investigator, a tool outside Themis or a person: judges it once, keeps it with why it was refused, and hands the one that goes next to governance |
| `internal/incident/governance` | Deciding each action by policy, approvals, handing approved actions to the executor, and putting a failed fix back |
| `internal/incident/governance/policy` | Policy rules: compiling them, and deciding an action with them |
| `internal/incident/verification` | Decides whether the fix worked |
| `internal/incident/reconciliation` | Notices targets changed outside Themis |
| `internal/incident/notification` | Tells people when an incident is handed to them |
| `internal/workload` | The target as Themis reads it: whether it is still the same object, its template, revisions, pods and replica counts. Everything else asks here |
| `internal/digest` | The action digest an approval is bound to, and the template hash it covers |
| `internal/server/httpapi`, `internal/server/grpcapi` | The HTTP API and the UI, and the investigators' gRPC API: who is calling, and translating to and from the incident's stages |
| `internal/server/identity` | Who is calling: accounts, sessions, sign-in, the initial admin password, and the gate that keeps Themis closed without an admin account |
| `internal/server/statuspage` | The status page: whether each part of an install works |
| `internal/config`, `internal/config/resources`, `internal/config/settings` | Configuration from the file, from custom resources, and from the Setup page |
| `internal/store` | PostgreSQL, and its migrations |
| `internal/platform/telemetry`, `internal/platform/token`, `internal/platform/redact` | Shared plumbing: logs, metrics, traces and Kubernetes API warnings; token files and bearer tokens; hiding secrets |
| `eval/`, `scripts/scenario` | Building each scenario pack case's bundle, the conformance suite and the pack scaffold; the recorded model replies the cases are replayed against |
| `internal/testutil` | Test helpers: a real API server, PostgreSQL, metrics |

A package uses another only through that package's own concept. A helper one package needs lives in it, unexported; one that unrelated packages need lives in `internal/platform`.

`scenarios/`, `capability` and `evidence` are where Themis grows. A new kind of incident is a scenario pack: one folder under `scenarios/`, mostly YAML, with its profile, its checks and the cases that prove it, and no change anywhere else; see [scenarios.md](scenarios.md). A new check is a YAML definition in a pack, no Go; see [checks.md](checks.md). A new fix is a package under `internal/catalog/capability/` that implements `capability.Capability`, plus one line in the catalog's constructor in `internal/catalog`; nothing else in the core changes.
