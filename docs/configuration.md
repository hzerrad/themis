# Configuration

Themis reads one YAML file, `themis.yaml`, for how it is installed, and with the Helm chart takes its profiles and global settings from [custom resources](#configuration-resources) that apply without a restart. The chart renders the file from its values; for a local run, see [`scripts/dev/themis.yaml`](../scripts/dev/themis.yaml). A commented example is in [`deploy/config/themis.example.yaml`](../deploy/config/themis.example.yaml), and [`deploy/config/themis.schema.json`](../deploy/config/themis.schema.json) is its JSON Schema: editors with YAML language support complete and validate the file as you type.

Unknown keys are errors, and Themis reports every problem at once and refuses to start rather than run half-configured.

## Top level

| Key | Required | Meaning |
|---|---|---|
| `enrolledNamespaces` | yes | Namespaces Themis may act on. Alerts about any other namespace are recorded as rejected. Each also needs RBAC, which the chart renders per namespace. |
| `intake.bearerTokenFile` | yes | File holding the token Alertmanager must send. Read on every request, so rotating the Secret needs no restart. |
| `intake.url` | no | Where Alertmanager reaches the intake, for the setup steps the Status page shows. The chart sets it. |
| `intake.tokenSecret` | no | The Secret, in Themis's namespace, whose `intake` key holds the token, for the same steps. The chart sets it. |
| `investigators.bearerTokenFile` | yes | Token the investigator must send to the core's gRPC API. |
| `settings.keyFile` | no | File holding the secret that encrypts secret settings made on the Setup page, such as the model's key. The chart generates it once, in the `themis-tokens` Secret, and keeps it across upgrades. Losing or changing it makes those settings unreadable: set them again. |
| `model.baseURL`, `model.name`, `model.apiKey` | no | What the install set for the investigator's model; the chart sets them from its values. The Setup page can't change these, and sets only what's left. |
| `investigators.disabled` | no | No investigator is installed. Themis collects evidence and runs its checks, then each incident waits (`awaiting_suggestion`) for a fix suggested by a person or a tool outside Themis, and closes without action if none comes within `externalProposals.window`. The chart sets it with `model.enabled: false`. |
| `api.bearerTokenFile` | yes | Admin token for the HTTP API and the UI. It keeps working when people sign in through OIDC. |
| `api.oidc` | no | Let people sign in through an OIDC provider; see [People and roles](#people-and-roles). |
| `api.access` | no | Roles for people who sign in, by username or group. |
| `executor.address`, `executor.bearerTokenFile` | no | Where `themis-executor` listens, and the token it requires. Without `executor`, approved actions are canceled, never executed. |
| `notifications.webhook.url` or `.urlFile` | no | Where escalations are POSTed. Use `urlFile` when the URL embeds a key. |
| `notifications.webhook.bearerTokenFile` | no | A token sent with each notice. |
| `notifications.externalURL` | no | Where people reach the UI, used for links in notices. |
| `externalProposals` | no | Who outside Themis may suggest fixes; see [below](#fixes-suggested-from-outside). |
| `resources.namespace` | no | Read profiles and global settings from custom resources, with this as Themis's own namespace. The chart sets it. |
| `profiles` | without `resources` | Incident classes, below. Set either this or `resources`, not both. |

## People and roles

**Before anyone signs in,** whoever opens Themis through `kubectl port-forward` to the core's setup port (8083) is an admin, recorded as `setup`. Kubernetes has already checked that they may port-forward to Themis, and the port listens only on the pod's own loopback address, so nothing else reaches it; the chart's NetworkPolicy blocks it too. The first time someone with the `admin` role signs in, with a Themis account or through OIDC, setup finishes for good: the port then serves Themis like any other address, with sign-in. The core refuses to start if `--setup-addr` isn't a loopback address. Turn the port off with the chart's `setup.enabled: false`.

**Themis accounts** are for teams without an identity provider. An admin makes them on the **Accounts** page, or during setup, where the first one is your own admin account. Each has a username, a password of at least 12 characters, and a role, which applies at once when changed. Passwords are kept only as argon2id hashes. Ten wrong passwords in a row lock an account for 15 minutes, and a failed sign-in never says whether the username exists. A new password ends the account's sessions everywhere. Admins can't change their own role or delete themselves, so no one locks themselves out by accident. Sign-in with a password needs HTTPS everywhere but `localhost`: its cookie is secure, so plain HTTP over a network can't sign in.

Without `api.oidc` or accounts, the UI and the API otherwise take only the emergency (admin) token, and every approval made with it is recorded as `admin`. With it, people sign in through your identity provider, and each approval and suggestion records who made it. Any OIDC provider works, such as Dex, Google, Okta, Keycloak or Entra ID.

```yaml
api:
  bearerTokenFile: /var/run/secrets/themis/admin
  oidc:
    issuerURL: https://idp.example.com
    clientID: themis
    clientSecretFile: /var/run/secrets/themis-oidc/client-secret
    redirectURL: https://themis.example.com/auth/callback
  access:
    - {role: admin, users: [alice@example.com]}
    - {role: approver, groups: [sre]}
    - {role: viewer, groups: [engineering]}
```

With the chart, set the `oidc` and `access` values instead; see the [chart README](../deploy/helm/themis/README.md#sign-people-in).

### `api.oidc`

| Key | Required | Meaning |
|---|---|---|
| `issuerURL` | yes | The provider's issuer, for example `https://accounts.google.com` |
| `clientID` | yes | The client ID the provider gave Themis |
| `clientSecretFile` | yes | File holding the client secret. Read at each sign-in, so rotating the Secret needs no restart. |
| `redirectURL` | yes | Themis's `/auth/callback` as the browser reaches it, below |
| `usernameClaim` | no | The ID token claim used as the username. Default `email`. |
| `groupsClaim` | no | The ID token claim listing the person's groups. Default `groups`. |
| `sessionTTL` | no | How long a sign-in lasts. Default `8h`. |
| `scopes` | no | Scopes to ask for. `openid` is always added. Default `email` and `profile`. |

**The redirect URL** is where the provider sends the browser back after sign-in. It must be Themis's `/auth/callback` at the address people type in their browser, not the in-cluster Service: behind an ingress at `https://themis.example.com`, it is `https://themis.example.com/auth/callback`; through `kubectl port-forward` on port 8090, it is `http://localhost:8090/auth/callback`. Register the same URL with the provider. Over HTTPS, the session cookie is sent only over HTTPS.

**Sign-in** asks the provider for `openid` plus `scopes`. Providers differ on groups: Dex and Okta send them only when asked for the `groups` scope, so add it there; Google and Entra ID refuse that scope, so leave it out and grant roles by username, or use groups the provider sends by default. When the username is the email, Themis refuses an address the provider says isn't verified. Themis reaches the provider when the first person signs in, so it starts even while the provider is down.

### `api.access`

Each entry gives a role to `users` (usernames), `groups`, or both. When someone matches several entries, the most trusted role wins.

| Role | Can |
|---|---|
| `viewer` | See incidents, the fixes Themis suggests, and their outcomes. Evidence is listed, but its content, such as logs, is hidden. |
| `responder` | Everything a viewer can, and also read the evidence itself, such as container logs |
| `approver` | Everything a responder can, and also suggest, approve and deny fixes |
| `admin` | Everything. Today this is the same as approver. |

**With no role,** someone who signs in but matches no entry sees nothing. Themis tells them who they're signed in as, and to ask a Themis admin to add them.

**The admin token** still works with OIDC on, and acts as an admin. Keep it as a way in when the provider is down. What it does is still recorded as `admin`, since anyone holding it looks the same.

**What is recorded:** an approval or denial records the person's username. A suggestion records its source as `person:<username>`, `person:setup` for someone who came in through port-forward before setup finished, or `admin-token` for the admin token, and policy rules see it as `proposal.source`. Each person has their own limit on suggestions.

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

`helm upgrade` updates the custom resource definitions before anything else. `helm uninstall` keeps them, and the resources you created.

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

**Agents** call the `ProposalService` gRPC API on port 9090: `ListIncidents`, `GetIncident`, and `SubmitProposal` with `incident_id` instead of `lease_id`. `GetIncident` lists the fixes the agent may suggest for that incident, each with what it does and the JSON Schema of its parameters. A suggestion names the fix in `capability_name` and gives `params` as that schema describes, for example `"capability_name": "deployment.rollback-to-revision", "params": {"revision": 1}`; parameters that don't fit are rejected with the reason. The older `rollback_to_revision` field still works. Each sends a Kubernetes ServiceAccount token issued for the audience `themis`, for example from a [projected volume](https://kubernetes.io/docs/concepts/storage/projected-volumes/#serviceaccounttoken), and Themis checks it with the API server. A token for another audience is refused. Add the agent's namespace to the chart's `networkPolicy.agentNamespaces`.

**HolmesGPT** suggests fixes through an adapter the chart installs. Point it at an existing HolmesGPT (0.25 or later) with its API key:

```bash
kubectl -n themis create secret generic holmes-key --from-literal=apiKey=...   # HolmesGPT's HOLMES_API_KEY
helm upgrade themis deploy/helm/themis -n themis --reuse-values \
  --set holmesgpt.enabled=true --set holmesgpt.url=http://holmes-holmes.holmes --set holmesgpt.apiKeySecret=holmes-key
```

The chart adds the adapter as a source with its own service account, limited to rollbacks in the enrolled namespaces, and lets it reach the core. Each incident waiting for a suggestion, or that Themis couldn't decide, is put to HolmesGPT once, with the checks and evidence Themis collected, but never container logs: HolmesGPT reads the cluster itself. Its answer must be JSON naming the incident's own Deployment and an earlier revision Themis collected; anything else, including a free-text answer, suggests nothing, and the adapter's log says why. What it does suggest is checked like any other suggestion, shows as from `holmesgpt` with the identity Themis verified, and needs approval like any fix. The adapter asks in two turns, first to investigate with HolmesGPT's tools, then for the verdict, and keeps what HolmesGPT ran and found as notes on the suggestion, for people, never its tools' output. **HolmesGPT is only as good as its model:** tried with a small local model (qwen3:8b), it ran no tools when given Themis's evidence, and when asked without it, every tool call it wrote failed. Use a model you'd trust with HolmesGPT on its own.

**People** send the same request as JSON to `POST /v1/incidents/{id}/proposals`, signed in with the [approver role](#people-and-roles) or with the admin token. A signed-in person is recorded as `person:<username>`. The admin token is recorded as `admin-token`, since anyone holding it looks the same.

**When a suggestion is taken:** while Themis investigates, for `window` after it couldn't decide, and, without an investigator, for `window` from when the incident opens. `window` defaults to 15 minutes with sources or without an investigator, and to none otherwise. During it the incident stays `inconclusive` rather than closing. A suggestion made while the investigation runs is used if the investigator has no accepted fix of its own, including when the investigator fails.

**What policy sees:** rules can read `proposal.source`, `proposal.authenticated` and `proposal.external`, all strings. To send every outside suggestion to a person, or refuse them, write a rule on `proposal.external == "true"`.

**Limits:**

- Themis governs what is suggested through it, not what an agent does with its own credentials.
- An incident outside a source's `namespaces` looks the same as one that doesn't exist: the source gets "not found", nothing is stored on the incident, and the core logs the attempt.
- `SubmitProposal` must cite evidence Themis collected and name the incident's exact target, UID included. Naming another target is recorded as a safety event.
- Evidence an agent reads is already redacted. Anything above its `ceiling` is withheld, and the response says how many items.
- In-cluster gRPC isn't encrypted yet, so these tokens rely on the NetworkPolicy, like the investigator's. Tokens issued for `themis` can't be replayed against other services.
- Themis remembers a token it accepted for a minute, or until it expires if sooner, and one it refused for 5 seconds. A ServiceAccount deleted meanwhile keeps access for up to that minute.
- Each address may try 30 tokens a minute that Themis hasn't recently accepted, and each source may make 120 `ListIncidents` and `GetIncident` calls a minute. Past either, the call gets `RESOURCE_EXHAUSTED`.

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
    checks: [rollout_recent, oom_killed_new_revision]   # optional; none means all
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

Deterministic facts, decided from the evidence by checks Themis ships with ([how a check is written](checks.md)). Each is `true`, `false` or `unknown`, and cites the evidence it used. `unknown` means the evidence cannot decide; policy should treat it as *not true*.

A profile runs every check unless its `checks` lists the ones to run. A name Themis doesn't have stops Themis from starting, or for a custom resource, keeps the profile from applying. Policy rules see only the checks the profile runs, so a profile whose rules read a check it doesn't run, or a check that doesn't exist, is refused the same way, with a message naming the rule. Without that, the rule couldn't be evaluated, and a rule that can't be evaluated denies: every fix would be refused.

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
        (predicates.rollout_recent != "true" ||
         predicates.failure_onset_after_rollout != "true" ||
         predicates.config_changed_before_failure != "false" ||
         (predicates.oom_killed_new_revision != "true" &&
          predicates.prior_revision_was_healthy != "true"))
      decision: deny
    - name: rollback-needs-approval
      when: action.capability == "deployment.rollback-to-revision" && action.restore != "true"
      decision: require_approval
```

These are the rules Themis ships. A rollback is forbidden unless a recent rollout looks to blame: the failures began after it, no ConfigMap changed before them, and either the new version runs out of memory or the version before it was healthy. A workload owned by GitOps is never changed. Anything allowed still needs a person's approval.

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

The **Status** page shows these steps with your install's address and alerts filled in, for kube-prometheus-stack or any other Alertmanager: admins click **Show how to connect Alertmanager** while no alerts arrive. By hand: add a receiver that posts to the core with the intake token. With the chart, the token is the `intake` key of the `themis-tokens` Secret: copy it into a Secret Alertmanager can mount.

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
