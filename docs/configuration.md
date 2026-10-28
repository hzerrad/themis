# Configuration

Themis reads one YAML file, `themis.yaml`. The Helm chart renders it from its values; for a local run, see [`hack/dev/themis.yaml`](../hack/dev/themis.yaml). A commented example is in [`deploy/config/themis.example.yaml`](../deploy/config/themis.example.yaml), and [`deploy/config/themis.schema.json`](../deploy/config/themis.schema.json) is its JSON Schema: editors with YAML language support complete and validate the file as you type.

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
| `profiles` | yes | Incident classes, below. |

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
      initial: [rollout_history, pod_states, target_events]
      allowed: [rollout_history, pod_states, target_events, container_logs_tail]
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
| `container_logs_tail` | The end of one container's logs, the pod needing attention most by default. Classified `sensitive`. |

No template reads pod specs, where environment variables often hold literal secrets. Every item is redacted before it is stored or sent: bearer and basic credentials, JWTs, private keys, cloud and API keys, `password=`-style pairs, and credentials in URLs.

### Predicates

Deterministic facts, computed by code from the evidence. Each is `true`, `false` or `unknown`, and cites the evidence it used. `unknown` means the evidence cannot decide; policy should treat it as *not true*.

| Predicate | True when |
|---|---|
| `rollout_recent` | The current revision rolled out within the last 6 hours |
| `failure_onset_after_rollout` | Every failure seen followed the rollout. A failure without a time makes it `unknown`. |
| `prior_revision_was_healthy` | Pods of the previous revision are ready and never failed |
| `oom_killed_new_revision` | A container of the current revision was OOMKilled |
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
        predicates.oom_killed_new_revision != "true" ||
        predicates.failure_onset_after_rollout != "true"
      decision: deny
    - name: rollback-needs-approval
      when: action.capability == "deployment.rollback-to-revision"
      decision: require_approval
```

- Rules are [CEL](https://cel.dev) expressions over `action.capability`, `action.risk`, `target.namespace`, `target.managed_by` (`argocd`, `flux` or empty), `predicates.<name>` (`"true"`, `"false"` or `"unknown"`), and `mode`.
- The first rule that matches decides. When none match, the action needs approval.
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
