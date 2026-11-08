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
| `themis-executor` | Get and patch Deployments in the namespaces `executor.namespaces` lists, with the fixes `executor.fixes` lists, or every fix when it lists none | Anything else; it acts only on an action ID, and rechecks everything itself |

Each API is behind its own bearer token: the alert webhook's, the investigator's, the executor's, and the admin token for the API and UI. The tokens are files, read on every request, so rotation needs no restart. People can also sign in to the UI through an OIDC provider, with a role each, so an approval records who gave it; see [People and roles](configuration.md#people-and-roles).

## An incident's life

```
received → investigating → diagnosed → proposed → awaiting_approval → authorized → executing → verifying → resolved
    │            │             │           │              │                                      │
 rejected   escalated    closed_no_action  recommended   awaiting_suggestion (expired)   escalated / contained_escalated
            canceled     self_resolved     closed_no_action  awaiting_suggestion (denied)   restoring → escalated
            awaiting_suggestion (referred)                       closed_no_action (denied and closed)
```

A denied fix leaves the incident `awaiting_suggestion`, waiting for another: one a person or an agent suggests, or one found by investigating it again. The new investigation is told which fixes people denied, and their reasons, as text for the model to weigh, never instructions, and it draws on the incident's token budget. A fix a person or an agent suggests that the checks or policy refuse leaves the incident waiting too, as does any refused fix, or an investigation that finds none, once a person has denied one or had the incident investigated again.

A fix policy allows that Themis can't apply is only recommended (`recommended`): the executor is off, `executor.namespaces` doesn't list the workload's namespace, `executor.fixes` doesn't list the fix, or Argo CD or Flux manages the workload. The incident page shows the exact change and why, with no Approve button, and how to make it: in Git under GitOps, and otherwise with the kubectl commands that make exactly that change, when kubectl can, never printing a value that looks secret. A person makes the change, and Themis judges it as any change made outside it: the incident closes as recovered once the workload is, and a change that doesn't help supersedes it. A person can also close it with no change.

Themis never closes an incident with no change on its own while its checks find the failure. When no fix was applied and none goes ahead, because the investigation found none or failed, the fix it proposed was refused, or nobody approved it in time, Themis refers the incident to a person and it waits (`awaiting_suggestion`) for the fix they suggest, saying which fix the checks support or what they found, with an escalation notice. No timer closes it while its symptoms are true: once it has waited for `externalProposals.window`, Themis decides them again each minute, and it closes on its own only when they are false ("Recovered before a fix."), or when its checks never found the failure. A fix Themis applied that didn't help still ends `escalated`: the incident's evidence and checks describe the workload before Themis changed it, so the person starts from the cluster as it is, and a new alert opens a new incident. That incident follows the earlier one: its evidence holds Themis's fix the workload runs, so the checks know the rollout was Themis's and that the revision before it failed.

When a fix is applied and the workload is unchanged or worse at the end of the verification window, Themis puts it back as it was before it acted (`restoring`), checks that Kubernetes rolled that version out, and hands the incident to a person (`escalated`). People then investigate the workload as they left it, not a mix of the incident and Themis's attempt. Putting it back is an action of its own: a rollback to the revision the fix replaced, with every check a fix has, decided by policy. It needs no approval unless a rule matching `action.restore == "true"` asks for one; then, or when Kubernetes no longer keeps that revision, Themis leaves its change in place and escalates at once. If someone changed the pod template after the fix, Themis leaves their change alone and escalates. If the restore can't be carried out, it escalates with its change still in place, so an incident is never left waiting. A fix that partly helped (`contained_escalated`) is kept, and an outcome changed by someone else during the window, or that Themis couldn't measure (`inconclusive`), is never put back. A fix judged by its profile's verification checks, such as a CPU raise or a rollback for throttling, is put back only when it left the workload worse: one whose pods are ready and stable while the failure goes on did no harm, and putting the old values back would only bring the failure back sooner, so it stays and the incident goes to a person.

Every transition is stored with its time, reason and actor, together with the job it starts, in one transaction: a crash never leaves a state change without its follow-up, or the reverse. Jobs run at least once, and every job is safe to repeat.

## What protects what

| Risk | Guard |
|---|---|
| A model told by a log line to act on another workload | The core accepts proposals only for the incident's exact target (UID included), only for capabilities one of its matching profiles offers, decided by those profiles' policies, and only citing evidence it collected. Anything else is stored as rejected, and a wrong target is recorded as a safety event. |
| A proposal that misdescribes the change | The action is derived from the live cluster, never from the proposal's text. The UI shows both side by side, with every field the action changes. |
| An approval reused, or for something that changed | An approval is bound to the action's digest, expires, and is consumed by the execution claim. The digest covers the target's UID, generation, a hash of its pod template, its field manager, and the hash of the exact template being restored, but not `resourceVersion`, which changes on every status update. |
| A change beyond what the install allows | The executor applies a fix only in a namespace `executor.namespaces` lists, and only a fix `executor.fixes` lists when it lists any. It reads both from its own flags and checks them before every write, whatever the database says was approved. Its Roles exist only in those namespaces. The core reads the same lists, and only recommends a fix the executor may not apply, so no one is asked to approve it. Every write it makes has the field manager `themis`, so the workload's managed fields name it. |
| A forged approval | The executor trusts the approval recorded in Themis's database. Anyone who can write that database can have it apply any change its fix list and Role allow. The checks against the live cluster still apply: the digest, the dry run and the `resourceVersion` precondition. Approvals carry no signature of the approver's identity yet. |
| Traffic between components read or changed inside the cluster | gRPC between the core, the investigator, the executor and agents is not encrypted, and carries their bearer tokens. Where the network plugin enforces NetworkPolicy, only the investigator, HolmesGPT's adapter and pods in `networkPolicy.agentNamespaces` reach the core's gRPC port, and only the core reaches the executor's. Anyone who can capture pod traffic can read the calls and their tokens. mTLS is planned. |
| The investigator reaching more than the model | It holds no cluster or database credentials. Its NetworkPolicy allows the core's gRPC API, DNS, and `investigator.egressCIDR` only on the model's port: `investigator.egressPorts`, or else the port in `investigator.model.baseURL`, 80 for `http://` without one, or 443. The default CIDR, `0.0.0.0/0`, allows every address on that port. Set the model's addresses to narrow it. |
| The cluster changing between approval and write | The executor rereads the target, and for a rollback the ReplicaSet of its revision, builds the template to write again, and recomputes the digest and that template's hash. Any difference makes the action `stale` and nothing is written, as does a revision Kubernetes no longer keeps. The write uses the fresh `resourceVersion` as a precondition, after a server-side dry run. |
| A write whose outcome is unknown | A timeout after the request was sent is recorded as `ambiguous` and escalated, never retried blindly. |
| Declaring success that is not one | Verification watches the outcome for a window, and decides the profile's verification checks again from what the workload did since the change, such as how often it is still throttled: ready pods aren't enough. A change by someone else during it, or a check it can't decide, makes the outcome `inconclusive`. |
| Fighting GitOps | Ownership is checked from the evidence and again live, from Argo CD and Flux markers and field managers. Governance never changes a target that is managed, or may be: it only recommends a fix policy allows. |
| Acting on a stale picture | Every 30 seconds Themis compares open incidents' targets with the template they were investigated against, up to 100 incidents a run, those it compared longest ago first. A change made elsewhere closes the incident as recovered only when its rollout is complete and ready, and the profiles' verification checks and symptoms, decided again from the new pods since the change, find the failure gone: ready pods aren't enough. A change that does not help, or whose effect a check can't tell, supersedes it with a new one. |
| Secrets reaching the model, the UI or the database | Evidence never includes pod specs, and every item is redacted. Themis never stores a pod template, whose environment can hold secrets, not even for a fix it only recommends. The UI shows what an action changes, hiding values that look secret by name or content. |
| Evidence posing as instructions | Evidence reaches the model fenced as untrusted data, with any tag-like text escaped. Text that looks like instructions, forged tool output or an order to make a fix is flagged in the core: the investigator, whichever it is, is told before it reads it, the approver is warned on the fix, and policy can read `evidence_has_instructions`. See [suspicious evidence](configuration.md#suspicious-evidence). |
| Evidence that talks a fix in or out | Logs, event messages and config values may raise a hypothesis, never settle a decision. A fix is refused unless a check its profile names to justify it is true, and when the checks support a fix the investigator didn't propose, a person is told. What operators read is built from checks and the fix's changes; the investigator's own summary is shown apart, as its note. See [justified fixes](configuration.md#justified-fixes). |
| A misconfiguration | The configuration is validated whole, policy rules are compiled at startup, and a rule that fails at runtime denies. |

## Data

PostgreSQL holds incidents and their transitions, alert states, rejected alerts, evidence (redacted, size-bounded), predicate results, investigations, proposals, safety events, actions, approvals, executions and verifications. Jobs run on [River](https://riverqueue.com), in the same database, so a state change and its job commit together.

An action keeps no pod template. It keeps what its fix needs to show the change and put it back:

| Fix | Stored |
|---|---|
| Rollback, partial rollback | The revision it replaces and the revision it goes back to, each by number, ReplicaSet and template hash; the hash of the template it writes; the changes people see, values that look secret withheld; for a partial rollback, the rollout's changes by name and the `kubectl set` commands for a person to run |
| Memory and CPU change | Each value it changes, before and after, and the hashes of the template before and after |
| Probe change | Each probe timing it changes, before and after, and the hashes of the template before and after |

A restore after a rollback rolls back to the ReplicaSet whose template has the hash of the one the rollback replaced. When Kubernetes no longer keeps that ReplicaSet, Themis leaves its change in place and escalates. A restore after a memory, CPU or probe change sets the values from before again. Upgrading from an earlier version replaces the templates it stored with these records, and cancels, saying why, a fix waiting for approval or execution that can't be recorded so.

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
| `internal/catalog/capability/rollback` | `deployment.rollback-to-revision`, and `deployment.roll-back-changes`: a partial rollback, which rolls back some of the latest rollout's changes and keeps the others |
| `internal/catalog/capability/probes` | `deployment.set-probes`: the timing of a container's existing liveness, readiness and startup probes |
| `internal/catalog/capability/resources` | `deployment.set-resources`: containers' memory and CPU requests and limits, checked against the namespace's LimitRanges and ResourceQuotas and the nodes' allocatable |
| `internal/catalog/check` | **What Themis decides from evidence**, such as "the crash started with the rollout": checks written in YAML with CEL expressions, compiled at start-up, and the variables they see. The checks themselves are in the scenario packs; see [checks.md](checks.md) |
| `internal/catalog/evidence` | **What Themis can read** about a target: pod states, events, logs, rollout history, memory and CPU use |
| `internal/catalog/evidence/metrics` | How much memory and CPU a workload's containers use, asked the same way of any metrics source: the Kubernetes metrics API, or a server answering Prometheus queries. A new source is one more adapter here; nothing that reads evidence changes |
| `internal/catalog/evidence/hint` | One line per piece of missing evidence: what is missing, and the change that lets Themis read it |
| `scenarios/` | **The incidents Themis handles**, one scenario pack per folder: its profile, its checks and the evaluation cases that prove it, built into the binary; see [scenarios.md](scenarios.md) |
| `internal/incident` | An incident and its states |
| `internal/incident/intake` | Alerts in, incidents out (`alertmanager/`, and `target/` to find the workload an alert is about) |
| `internal/incident/investigation` | An incident's investigation: prepares it, hands it to an investigator under a lease, reads the evidence it asks for within its profile and budget, records what it couldn't have, and ends it with the state its outcome leads to; waits for a suggested fix when there is none, and investigates again when a person asks |
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
