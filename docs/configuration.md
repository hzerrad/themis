# Configuration

Themis reads one YAML file, `themis.yaml`, for how it is installed, and with the Helm chart takes its profiles and global settings from [custom resources](#configuration-resources) that apply without a restart. The chart renders the file from its values; for a local run, see [`scripts/dev/themis.yaml`](../scripts/dev/themis.yaml). A commented example is in [`deploy/config/themis.example.yaml`](../deploy/config/themis.example.yaml), and [`deploy/config/themis.schema.json`](../deploy/config/themis.schema.json) is its JSON Schema: editors with YAML language support complete and validate the file as you type.

Unknown keys are errors, and Themis reports every problem at once and refuses to start rather than run half-configured.

## Top level

| Key | Required | Meaning |
|---|---|---|
| `enrolledNamespaces` | yes | Namespaces Themis may act on. Alerts about any other namespace are recorded as rejected. Each also needs RBAC, which the chart renders per namespace. |
| `intake.bearerTokenFile` | yes | File holding the token Alertmanager must send. Read on every request, so rotating the Secret needs no restart. |
| `investigators.bearerTokenFile` | yes | Token the investigator must send to the core's gRPC API. |
| `api.bearerTokenFile` | yes | Admin token for the HTTP API and the UI. |
| `executor.address`, `executor.bearerTokenFile` | no | Where `themis-executor` listens, and the token it requires. Without `executor`, approved actions are canceled, never executed. |
| `notifications.webhook.url` or `.urlFile` | no | Where escalations are POSTed. Use `urlFile` when the URL embeds a key. |
| `notifications.webhook.bearerTokenFile` | no | A token sent with each notice. |
| `notifications.externalURL` | no | Where people reach the UI, used for links in notices. |
| `externalProposals` | no | Who outside Themis may suggest fixes; see [below](#fixes-suggested-from-outside). |
| `resources.namespace` | no | Read profiles and global settings from custom resources, with this as Themis's own namespace. The chart sets it. |
| `profiles` | without `resources` | Incident classes, below. Set either this or `resources`, not both. |

## Configuration resources

With the chart, configuration lives in two custom resources, so it can be applied with `kubectl` or kept in Git. Changes apply without a restart.

### IncidentProfile

Its spec is a [profile](#profiles), without `name`: the resource's name is the profile's. Where it lives decides what it applies to:

| Namespace | Applies to | An incident records it as |
|---|---|---|
| Themis's own | Every enrolled namespace | `rollout-regression` |
| An enrolled namespace | Only that namespace. On equal priority, it beats a profile from Themis's namespace. | `shop/rollout-regression` |
| Any other | Nothing, and its status says so | |

Anyone who can write an `IncidentProfile` in a namespace chooses what Themis may read and propose there, so grant it with Kubernetes RBAC as you would other write access. A team can't reach another namespace, and it can't go past the `ThemisConfig` ceiling.

The chart's `profiles` value installs resources in Themis's namespace.

### ThemisConfig

One per cluster, named `themis`, and optional. The chart's `themisConfig` value installs it.

```yaml
apiVersion: themis.dev/v1alpha1
kind: ThemisConfig
metadata:
  name: themis
spec:
  enrolledNamespaces: [shop]   # narrows the chart's list; it can't widen it
  autonomyCeiling: observe     # stops Themis making changes anywhere
  externalProposals: {}        # see "Fixes suggested from outside"
```

`enrolledNamespaces` can only narrow the chart's list, because that list is what grants Themis its permissions, and Themis never creates permissions while running. A namespace outside it is ignored and the status says so.

### Status

Each resource says whether it is in use:

```bash
kubectl get incidentprofiles -A
kubectl -n shop get incidentprofile rollout-regression -o jsonpath='{.status.conditions}'
```

A resource that fails validation, such as a policy rule that doesn't compile or an unknown evidence template, is refused with the reason in its status. Its previous version keeps applying until the core restarts. Two profiles with the same priority in the same place can't both apply: the older one stays.

What a change does to work already under way:

- A new investigation or decision uses the configuration in force at that moment, and records the digest of the policy it used.
- A fix already waiting for approval keeps the decision it got.
- If a profile is removed, an incident using it that hasn't reached a decision stops and says so.

`helm upgrade` doesn't update custom resource definitions. When a release changes them, apply them first: `kubectl apply -f deploy/helm/themis/crds`.

## Fixes suggested from outside

An agent or a person can suggest a fix for an incident. A suggestion authorizes nothing: it passes the same checks as the investigator's own, then policy and approval. What fails a check is kept, with the reason.

```yaml
externalProposals:
  window: 15m
  sources:
    - serviceAccount: sre-agents/triage   # namespace/name
      namespaces: [shop]                  # incidents it may read and suggest fixes for
      capabilities: [deployment.rollback-to-revision]
      perMinute: 6
      ceiling: internal                   # sensitive also lets it read container logs
```

**Agents** call the `ProposalService` gRPC API on port 9090: `ListIncidents`, `GetIncident`, and `SubmitProposal` with `incident_id` instead of `lease_id`. Each sends a Kubernetes ServiceAccount token issued for the audience `themis`, for example from a [projected volume](https://kubernetes.io/docs/concepts/storage/projected-volumes/#serviceaccounttoken), and Themis checks it with the API server. A token for another audience is refused. Add the agent's namespace to the chart's `networkPolicy.agentNamespaces`.

**People** send the same request as JSON to `POST /v1/incidents/{id}/proposals` with the admin token. That token is shared, so the record proves the token was used, not who used it.

**When a suggestion is taken:** while Themis investigates, and for `window` after it couldn't decide. `window` defaults to 15 minutes with sources and to none without. During it the incident stays `inconclusive` rather than closing. A suggestion made while the investigation runs is used if the investigator has no accepted fix of its own.

**What policy sees:** rules can read `proposal.source`, `proposal.authenticated` and `proposal.external`, all strings. To send every outside suggestion to a person, or refuse them, write a rule on `proposal.external == "true"`.

**Limits:**

- Themis governs what is suggested through it, not what an agent does with its own credentials.
- `SubmitProposal` must cite evidence Themis collected and name the incident's exact target, UID included. Naming another target is recorded as a safety event.
- Evidence an agent reads is already redacted. Anything above its `ceiling` is withheld, and the response says how many items.
- In-cluster gRPC isn't encrypted yet, so these tokens rely on the NetworkPolicy, like the investigator's. Tokens issued for `themis` can't be replayed against other services.

## Profiles

A profile says which alerts belong to an incident class, what an investigation may look at, what it may propose, and how proposals are decided.

```yaml
profiles:
  - name: rollout-regression
    priority: 100
    match:
      alertNames: [KubePodCrashLooping, KubePodOOMKilled]
      labels: {severity: warning}        # optional; all must match
      targetKinds: [Deployment.apps]
    evidence:
      initial: [rollout_history, pod_states, target_events, referenced_config]
      allowed: [rollout_history, pod_states, target_events, referenced_config, container_logs_tail]
    capabilities: [deployment.rollback-to-revision]
    investigation: {maxSteps: 8, maxEvidenceRequests: 6, timeout: 5m}
    policy: {...}
    verification: {window: 5m, stability: 2m}
```

### Matching alerts

An alert is resolved to the workload it is about: the object its labels name (`deployment`, `statefulset`, `daemonset`, `job_name`, `persistentvolumeclaim`, `pod`), followed up controller owner references to the top. A `themis_target=<Kind.group>/<name>` label overrides that. The highest-priority profile whose `alertNames`, `labels` and `targetKinds` all match wins; priorities must be unique. Alerts that match no profile are recorded as rejected, with the reason.

Alerts with the same profile and target join one open incident.

### Evidence templates

The investigator cannot query the cluster: it can only ask for a template by name. `initial` templates are collected before it starts; `allowed` ones it may request.

| Template | What it reads |
|---|---|
| `rollout_history` | The Deployment's rollout progress and conditions, when its template last changed and by which field manager, GitOps ownership markers, and each ReplicaSet revision with its images and resource limits |
| `pod_states` | Each pod: its revision, readiness, and each container's current state and last termination, where `OOMKilled` shows |
| `target_events` | Recent events of the Deployment, its ReplicaSets and its pods |
| `referenced_config` | The ConfigMaps and Secrets the pod template refers to, and when each ConfigMap last changed. Names and times only. Secrets are listed by name and never read. |
| `container_logs_tail` | The end of one container's logs, the pod needing attention most by default. Classified `sensitive`. |

No template returns pod specs, where environment variables often hold literal secrets. Every item is redacted before it is stored or sent: bearer and basic credentials, JWTs, private keys, cloud and API keys, `password=`-style pairs, and credentials in URLs.

### Predicates

Deterministic facts, computed by code from the evidence. Each is `true`, `false` or `unknown`, and cites the evidence it used. `unknown` means the evidence cannot decide; policy should treat it as *not true*.

| Predicate | True when |
|---|---|
| `rollout_recent` | The current revision rolled out within the last 6 hours |
| `failure_onset_after_rollout` | Every failure seen followed the rollout. A failure without a time makes it `unknown`. |
| `prior_revision_was_healthy` | Pods of the previous revision are ready and never failed |
| `oom_killed_new_revision` | A container of the current revision was OOMKilled |
| `config_changed_before_failure` | A ConfigMap the pod template refers to changed more than a minute after the rollout and no later than the first failure, so rolling back might not undo the cause. `unknown` when a required ConfigMap is missing or doesn't say when it changed. Secrets are never read, so a change to one can't be seen. |
| `gitops_managed` | The target carries Argo CD or Flux markers, or is written by their controllers. Governance also checks this live. |

### Capabilities

`deployment.rollback-to-revision` restores the pod template of a prior ReplicaSet revision, as `kubectl rollout undo` does, with the target's resource version as a precondition and a server-side dry run first. A profile without `capabilities` is diagnosis only.

### Investigation

| Key | Default | Meaning |
|---|---|---|
| `maxSteps` | 8 | Model steps per investigation |
| `maxEvidenceRequests` | 6 | Templates the investigator may request beyond the initial ones |
| `timeout` | 5m | Deadline for the whole investigation |

A failed investigation is retried twice, 30 seconds and then a minute later. After that the incident is escalated.

### Policy

Policy decides every derived action: `deny`, `require_approval`, or `allow`.

```yaml
policy:
  mode: guarded            # guarded (default) or observe
  approvalTimeout: 30m
  rules:
    - name: gitops-owns-the-target
      when: predicates.gitops_managed != "false"
      decision: deny
    - name: rollback-needs-a-bad-rollout
      when: >-
        action.restore != "true" &&
        (predicates.oom_killed_new_revision != "true" ||
         predicates.failure_onset_after_rollout != "true" ||
         predicates.config_changed_before_failure != "false")
      decision: deny
    - name: rollback-needs-approval
      when: action.capability == "deployment.rollback-to-revision" && action.restore != "true"
      decision: require_approval
```

- Rules are [CEL](https://cel.dev) expressions over `action.capability`, `action.risk`, `action.restore` (`"true"` when Themis puts back what it changed, after a fix that didn't help), `target.namespace`, `target.managed_by` (`argocd`, `flux` or empty), `predicates.<name>` (`"true"`, `"false"` or `"unknown"`), `proposal.source`, `proposal.authenticated`, `proposal.external`, and `mode`.
- The first rule that matches decides. When none match, a fix needs approval, and putting things back doesn't: it returns the workload to how a person left it. A rule matching `action.restore == "true"` can ask for approval or forbid it, and Themis then escalates with its change left in place.
- A rule that fails to evaluate denies. Policy never fails open.
- In **guarded** mode `allow` still needs approval: no action runs without a human. In **observe** mode every action is denied, so Themis only diagnoses and recommends.
- An approval not given within `approvalTimeout` expires, and the incident is escalated.

Every rule is compiled at startup; a rule that does not compile to a boolean stops Themis from starting.

### Verification

| Key | Default | Meaning |
|---|---|---|
| `window` | 5m | How long Themis watches the target after acting |
| `stability` | 2m | How long every replica must stay ready with no new restarts or OOMKills |

The outcome is `resolved` (healthy and stable), `contained` (a complete rollout, but not stable), `unchanged`, `worsened` (fewer ready replicas than before), or `inconclusive`: the template was changed by someone else during the window. Anything but `resolved` escalates the incident.

## Connecting your Alertmanager

Add a receiver that posts to the core with the intake token. With the chart, the token is the `intake` key of the `themis-tokens` Secret: copy it into a Secret Alertmanager can mount.

```yaml
receivers:
  - name: themis
    webhook_configs:
      - url: http://themis-core.themis.svc:8080/v1/intake/alertmanager
        send_resolved: true
        http_config:
          authorization:
            type: Bearer
            credentials_file: /etc/alertmanager/themis/token
route:
  routes:
    - receiver: themis
      matchers: [alertname=~"KubePodCrashLooping|KubePodOOMKilled"]
      group_by: [alertname, namespace, pod]
      continue: true
```

`send_resolved: true` matters: when every alert of an incident resolves before Themis acts, the incident closes as `self_resolved`.

## Escalation notices

With `notifications.webhook` set, every escalation is POSTed as JSON:

```json
{
  "event": "incident.escalated",
  "incident": {
    "id": "01a1837f-…", "state": "escalated", "from": "investigating",
    "reason": "investigation failed: INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE",
    "profile": "rollout-regression",
    "target": {"group": "apps", "kind": "Deployment", "namespace": "shop", "name": "checkout", "uid": "…"},
    "at": "2026-10-28T17:13:54Z",
    "url": "https://themis.example.com/#/incidents/01a1837f-…"
  }
}
```

`event` is `incident.escalated` or `incident.contained_escalated`. Anything but a 2xx answer is retried with backoff for about a day. Notices never carry evidence.
