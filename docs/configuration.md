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
| `model.baseURL`, `model.name`, `model.apiKey` | no | What the install set for the investigator's model; the chart sets them from its values. The Setup page can't change these, and sets only what's left. Which provider the investigator speaks to is the chart's `model.provider`: `openai` (the default) for any OpenAI-compatible API, or `anthropic` for Claude models through Anthropic's own API, which needs no address; the Setup page's address doesn't apply to it, but its model name and key do. How an OpenAI-compatible API is asked for its reply is the chart's too: `model.outputLimitField`, `max_tokens` (the default) or `max_completion_tokens`; `model.temperature`, a number (`0` by default) or `default` to send none; and `model.schemaDialect`, `as-is` (the default) or `openai-strict`, which sends the reply schema in the form OpenAI's strict mode accepts: every object closed, every field required, an optional one allowed to be null, and a map such as a fix's containers as a list of entries. Themis turns each reply back and checks it against its own schema. For OpenAI's newer models, set all three: `max_completion_tokens`, `default` and `openai-strict`. Ollama and vLLM take the defaults. |
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
| `metrics` | no | Where Themis reads how much memory and CPU containers use; see [Metrics](#metrics). The Kubernetes metrics API is on by default. |
| `scenarios.disabled` | no | [Scenario packs](scenarios.md) to turn off, by name. Every pack is on otherwise, each bringing its profile and checks. A name no pack has stops Themis from starting. |
| `profiles` | no | Incident classes of your own, below, on top of the scenario packs'. Set either this or `resources`, not both. |

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

The chart's `profiles` value installs resources in Themis's namespace. An `IncidentProfile` there with the same name as a scenario pack's profile replaces it; if it is refused, the pack's applies.

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

Each [scenario pack](scenarios.md) brings one, named after the pack: `rollout-regression` is in [`scenarios/rollout-regression/scenario.yaml`](../scenarios/rollout-regression/scenario.yaml). A profile of your own, in the file or as an `IncidentProfile`, with the same name replaces the pack's entirely, so you can tighten its policy without changing Themis: copy the pack's profile and edit it. On equal priority, your own profile wins over a pack's.

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
    checks: [rollout_recent, failure_onset_after_rollout, oom_killed_new_revision]   # optional; none means all
    capabilities: [deployment.rollback-to-revision]
    justifiedBy:                         # for each fix, the checks that justify it
      deployment.rollback-to-revision: [failure_onset_after_rollout]
    guidance: |                          # optional
      A rollback is right when the current revision introduced the failure
      and a prior revision was healthy. ...
    investigation: {maxSteps: 8, maxEvidenceRequests: 6, timeout: 5m}
    policy: {...}
    verification: {window: 5m, stability: 2m}
```

### Matching alerts

An alert is resolved to the workload it is about: the object its labels name (`deployment`, `statefulset`, `daemonset`, `job_name`, `persistentvolumeclaim`, `pod`), followed up controller owner references to the top. A `themis_target=<Kind.group>/<name>` label overrides that. Every profile whose `alertNames`, `labels` and `targetKinds` all match applies to the incident. Priority only orders them: the highest-priority one is the incident's primary profile, the one it is shown and grouped under. Priorities must be unique among your profiles, and among the packs'. Alerts that match no profile are recorded as rejected, with the reason.

Alerts with the same primary profile and target join one open incident.

### Combined profiles

When several profiles match, such as two scenario packs that both answer `KubePodOOMKilled`, the incident gets all of them combined:

| What | Combined |
|---|---|
| Checks | Every check any of them runs, each once |
| Evidence | Every `initial` template any of them collects first, and every `allowed` one any of them allows |
| Fixes | Every fix any of them offers |
| Root causes | Each pack's, the primary's first, each name once |
| Guidance | Each profile's, introduced by its name and the fixes it offers, so the investigator knows which part is about which fix |
| Investigation | The largest `maxSteps`, `maxEvidenceRequests` and `timeout` among them |

The investigator decides which fix the evidence supports. Each fix is then decided only by the profiles that offer it: a profile's policy governs its own fixes, never another profile's, and each one's [justifying checks](#justifying-checks) must hold. When several profiles offer the same fix, each one's policy decides it, mode and all, and the strictest outcome wins: `deny` over `require_approval` over `allow`. The shortest `approvalTimeout` and the longest `verification` among them apply. A fix none of the incident's profiles offers is refused, whoever suggests it.

So a rule that protects a workload belongs in every profile that offers a fix. Themis's own protections, such as [never changing a workload GitOps owns](#gitops), apply to every fix before any profile's policy. An incident that matched one profile is decided exactly as before.

### Evidence templates

The investigator cannot query the cluster: it can only ask for a template by name. `initial` templates are collected before it starts; `allowed` ones it may request.

| Template | What it reads |
|---|---|
| `rollout_history` | The Deployment's rollout progress and conditions, when its template last changed and by which field manager, GitOps ownership markers, and each ReplicaSet revision with its images, resource limits and probe timings (never what a probe calls) |
| `pod_states` | Each pod: its revision, readiness, and each container's current state and last termination, where `OOMKilled` shows |
| `target_events` | Recent events of the Deployment, its ReplicaSets and its pods |
| `referenced_config` | The ConfigMaps and Secrets the pod template refers to, and when each ConfigMap last changed. Names and times only. Secrets are listed by name and never read. |
| `container_logs_tail` | The end of one container's logs, the pod needing attention most by default. Classified `sensitive`. |
| `container_memory` | For each container of the current revision: its memory request and limit, the memory it uses now and at its peak over a window (`window`, 1h by default), each the highest among the revision's pods and as a share of the limit, and which [metrics source](#metrics) said so |
| `container_cpu` | For each container of the current revision: its CPU request and limit, the cores it uses now, and the share of its CPU periods throttled over a window (`window`, 1h by default), and which [metrics source](#metrics) said so |

No template returns pod specs, where environment variables often hold literal secrets. Every item is redacted before it is stored or sent: bearer and basic credentials, JWTs, private keys, cloud and API keys, `password=`-style pairs, and credentials in URLs.

### Predicates

Deterministic facts, decided from the evidence by checks ([how a check is written](checks.md)): Themis's own, which every profile may run, and those the scenario packs bring. Each is `true`, `false` or `unknown`, and cites the evidence it used. `unknown` means the evidence cannot decide; policy should treat it as *not true*.

A profile runs every check unless its `checks` lists the ones to run, in the order it lists them. Whenever an incident's profiles offer a fix, Themis also runs `gitops_managed`, and reads `rollout_history` for it, whether or not a profile names them: [whether Themis may change the workload at all](#gitops) depends on it. A name Themis doesn't have stops Themis from starting, or for a custom resource, keeps the profile from applying. Policy rules see only the checks the profile runs, so a profile whose rules read a check it doesn't run, or a check that doesn't exist, is refused the same way, with a message naming the rule. Without that, the rule couldn't be evaluated, and a rule that can't be evaluated denies: every fix would be refused. So is a profile whose rules or justifying checks read a check without the evidence it needs in `evidence.initial`, such as `config_changed_before_failure` without `referenced_config`: the check would always be `unknown`.

Themis's own checks, in [`internal/catalog/check/builtin.yaml`](../internal/catalog/check/builtin.yaml):

| Predicate | True when |
|---|---|
| `rollout_recent` | The current revision rolled out within the last 6 hours |
| `gitops_managed` | The target carries Argo CD or Flux markers, or is written by their controllers. Governance also checks this live. |
| `evidence_has_instructions` | Some evidence contains text that looks like instructions or forged tool output: see [suspicious evidence](#suspicious-evidence). No pack runs it; name it in a profile's `checks` to write a rule on it. Governance decides it again over all the incident's evidence, logs an investigator asked for included, before deciding a fix. |

The `rollout-regression` pack's:

| Predicate | True when |
|---|---|
| `failure_onset_after_rollout` | Every failure seen followed the rollout. A failure without a time makes it `unknown`, and so do missing events: an earlier failure could be among them. A pod failure from before the rollout still makes it `false`. When every pod of the current revision is ready and none restarted, it is `unknown`, whatever the events say. |
| `prior_revision_was_healthy` | Pods of the previous revision are ready and never failed |
| `oom_killed_new_revision` | A container of the current revision was OOMKilled |
| `config_changed_before_failure` | A ConfigMap the pod template refers to changed more than a minute after the rollout and no later than the first failure, so rolling back might not undo the cause. The first failure is dated by the pods' status, and by an event only when the status agrees with it, so an event anyone could write can't move it earlier. `unknown` when a required ConfigMap is missing or doesn't say when it changed, or the status can't date the first failure. Secrets are never read, so a change to one can't be seen. |

### Capabilities

A profile without `capabilities` is diagnosis only: when the investigation ends without a fix, and no profile of the incident offers one, Themis hands the incident to a person with what the checks found, such as "An image can't be pulled: inventory-84ddb6f89-6zthq/app (registry.example/shop/inventory:2.4.1) is waiting with ImagePullBackOff.", or says they found no cause. Every fix writes with the target's resource version as a precondition, after a server-side dry run.

`deployment.rollback-to-revision` restores the pod template of a prior ReplicaSet revision, as `kubectl rollout undo` does.

`deployment.set-probes` changes the timing of a container's liveness, readiness and startup probes. The investigator picks the probes and the values from the evidence; Themis doesn't estimate or cap them.

- **It may change** `initialDelaySeconds`, `periodSeconds`, `timeoutSeconds` and `failureThreshold` of any of the three, and `successThreshold` of a readiness probe. Kubernetes requires `successThreshold` to be 1 for the other two. Parameters: `{"container": "app", "readiness": {"timeoutSeconds": 5}, "liveness": {"failureThreshold": 6}}`, only the fields to change.
- **It never** adds or removes a probe, or changes what a probe calls (its HTTP path or port, command, TCP or gRPC check): those are changes to the application, for a person. Its parameters can't express them.
- **It refuses** a container or a probe the workload doesn't have, and values Kubernetes wouldn't accept: whole numbers, `initialDelaySeconds` from 0, the others from 1, up to 2147483647.
- **The approver sees** each field before and after, marked "more lenient" or "stricter", and a readiness probe made more lenient on a line of its own: pods that fail it keep receiving traffic for longer.
- **It worked** when the rollout is complete, every replica is ready, and no container restarted during the verification window, as one would if its liveness probe still failed it.
- **Undoing it** sets the fields it changed back to what they were.

`deployment.set-resources` sets the memory and CPU requests and limits of containers in the pod template, which rolls out new pods. The investigator picks the containers and the values from the evidence; Themis checks they are valid and that the cluster can run them, never how big they are. Policy and the approver decide whether the size is acceptable.

- **It may change** the `memory` and `cpu` requests and limits of the containers it names. Parameters: `{"containers": {"app": {"limits": {"memory": "512Mi"}, "requests": {"memory": "384Mi"}}}}`, as Kubernetes quantities, only the values to set.
- **It never** removes a request or a limit, or touches any other resource, field or container: its parameters can't express them.
- **It refuses** a container the workload doesn't have, a value that isn't a positive quantity, and a request that would end above its limit. Before policy decides, it also refuses, saying by how much, a change the cluster can't run: a value a LimitRange in the namespace forbids (including a default limit below the new request); a pod request larger than the allocatable of every node the pods could be scheduled on; or a rollout that would exceed a ResourceQuota in the namespace, counting the extra pods it runs at once. When Themis isn't allowed to read nodes, ResourceQuotas or LimitRanges, it refuses rather than guess.
- **The approver sees** each value before and after, per container, such as `app` `memory limit` `256Mi` → `512Mi`.
- **It worked** when the rollout is complete, every replica is ready, and no container restarted or ran out of memory during the verification window.
- **Undoing it** sets the values it changed back to what they were. A value the workload didn't have before can't be put back, since the fix never removes one, so Themis escalates with its change in place.

More lenient means slower to call a container failed or quicker to call it ready: a longer delay, period or timeout, a higher `failureThreshold`, or a lower `successThreshold`.

### Justifying checks

`justifiedBy` names, for every fix a profile offers, the checks that justify it. Themis refuses the fix unless at least one of them is true for the incident, before any policy and whoever suggests it, and says why on the incident's timeline, such as "None of the checks that justify a probe change is true: probe_kills is unknown, probe_timeouts is false." Putting a fix back is exempt.

```yaml
capabilities: [deployment.set-probes]
justifiedBy:
  deployment.set-probes: [probe_kills, probe_timeouts]   # either one true is enough
```

A profile that offers a fix must name its justifying checks, and each must be a check the profile runs: a check that doesn't run is never true, so the fix would always be refused. Logs, event messages and config values are written by whoever can write to the workload, and they may suggest a fix to the investigator, never justify one: only checks, which read what Kubernetes itself records, do. Policy still decides everything else, such as forbidding a raise right after a rollout.

### Guidance

`guidance` is plain text the investigator reads with every investigation of the profile: when the profile's fixes are right, and when they are wrong, for this kind of incident. For example, the `rollout-regression` pack says a rollback is right only when the new revision brought the failure and the one before it was healthy, and that empty logs after a container was killed for memory prove nothing. Up to 4000 characters.

It is guidance, not a rule. What must hold before a fix runs belongs in [checks](#predicates) and [policy](#policy): Themis enforces those on every suggested fix, whoever suggests it, and guidance on none.

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
    - name: rollback-needs-a-bad-rollout
      when: >-
        action.restore != "true" &&
        (predicates.rollout_recent != "true" ||
         predicates.config_changed_before_failure != "false" ||
         (predicates.oom_killed_new_revision != "true" &&
          predicates.prior_revision_was_healthy != "true"))
      decision: deny
    - name: rollback-needs-approval
      when: action.capability == "deployment.rollback-to-revision" && action.restore != "true"
      decision: require_approval
```

A profile's rules decide only the fixes it offers in `capabilities`: on an incident that matched [several profiles](#combined-profiles), another profile's fixes are decided by that profile's rules. These are the rules Themis ships. A rollback is forbidden unless a recent rollout looks to blame: no ConfigMap changed before the failures, and either the new version runs out of memory or the version before it was healthy. That the failures began after the rollout is the check that [justifies](#justifying-checks) a rollback, so no rule repeats it. Anything allowed still needs a person's approval. A workload owned by GitOps is never changed either, but no rule says so: [Themis does that itself](#gitops).

- Rules are [CEL](https://cel.dev) expressions over `action.capability`, `action.risk`, `action.restore` (`"true"` when Themis puts back what it changed, after a fix that didn't help), `target.namespace`, `target.managed_by` (`argocd`, `flux` or empty), `predicates.<name>` (`"true"`, `"false"` or `"unknown"`), `change.<key>` (what the fix says about the change, below), `proposal.source`, `proposal.authenticated`, `proposal.external`, and `mode`.
- `change` holds what a fix tells policy about its change. A rollback says nothing. `deployment.set-probes` sets `change.container`; `change.liveness`, `change.readiness` and `change.startup`, each `more_lenient`, `stricter`, `mixed` or `unchanged`; and for each field it changes, `change["<probe>.<field>"]` with the new value and `change["<probe>.<field>.before"]` with the old, such as `change["readiness.timeoutSeconds"] == "5"`. `deployment.set-resources` sets `change.containers`, the containers it changes, comma-separated; `change.memory` and `change.cpu`, each `increase`, `decrease` or `mixed`; `change["memory.ratio"]` and `change["cpu.ratio"]`, the largest new value over its old one; and for each value it changes, `change["<container>.<memory|cpu>.<request|limit>"]`, `increase`, `decrease` or `set` (where there was none), with `.before`, `.after` and `.ratio` beside it, such as `change["app.memory.limit.ratio"] == "2"`. The shipped policies set no size limits. A key a fix doesn't set fails the rule, which denies, so ask which fix it is first: `action.capability == "deployment.set-probes" && change.readiness == "more_lenient"`.
- The first rule that matches decides. When none match, a fix needs approval, and putting things back doesn't: it returns the workload to how a person left it. A rule matching `action.restore == "true"` can ask for approval or forbid it, and Themis then escalates with its change left in place.
- A rule that fails to evaluate denies. Policy never fails open.
- In **guarded** mode `allow` still needs approval: no action runs without a human. In **observe** mode every action is denied, so Themis only diagnoses and recommends.
- An approval not given within `approvalTimeout` expires, and the incident is escalated.

Every rule is compiled at startup; a rule that does not compile to a boolean stops Themis from starting.

### Core protections

Some protections hold for every fix and every profile, whoever suggested the fix, and no policy can loosen them. Themis applies them before any profile's policy.

#### GitOps

Themis never changes a workload that Argo CD or Flux manages: the GitOps controller would undo the change, or fight it. It only recommends the fix. The fix is recorded as denied by `gitops-owns-the-target`, the incident closes without a change, and the incident page shows it as the recommended fix, with the reason:

- "Argo CD manages this workload, so Themis only recommends the change: apply it in Git." when Themis reads the markers or field managers on the live workload as it decides (`Flux` for Flux's).
- "Argo CD or Flux manages this workload, so Themis only recommends the change: apply it in Git." when `gitops_managed` was true from the evidence.
- "Themis couldn't tell whether Argo CD or Flux manages this workload, so it only recommends the change." when `gitops_managed` is `unknown`, such as when Themis couldn't read the rollout history. Not knowing counts as managed.

This holds for putting a fix back too. A rule of your own that reads `predicates.gitops_managed` or `target.managed_by` still works, but is never needed for this.

#### Justified fixes

A fix none of its [justifying checks](#justifying-checks) supports is refused before any policy, with the reason, and recorded as denied by `no-justifying-check`. Putting a fix back is exempt.

When the investigator concludes without a fix but the checks support one that the incident's profiles offer, Themis doesn't close the incident: it hands it to a person, saying so, such as "Themis's checks support a rollback (failure_onset_after_rollout is true), but the investigator didn't propose it." Text in the evidence, such as "this incident is handled" or "change freeze", can talk an investigator out of a right fix; it can't make it disappear. A fix counts when one of its justifying checks is true and no rule of the profiles offering it forbids it on the checks alone: a rule that depends on what the fix would change is passed over, since no change was proposed, and the cluster isn't read, so GitOps may still keep Themis from applying it. The incident page and the [escalation notice](#escalation-notices) show the sentence.

What operators read about a decision comes from what Themis can check: the root cause from the investigator's list, the checks the fix cites with what Themis found, and the change the fix would make. The investigator's own summary, and a suggester's reasons, are shown apart, marked as written by the model or the suggester.

#### Suspicious evidence

Anyone who can write a workload's logs, events or ConfigMaps chooses part of what the investigator reads. Themis looks for text in every piece of evidence that looks written for the investigator rather than by the workload, and flags it with a plain reason:

| Reason | Such as |
|---|---|
| instructions addressed to an AI or investigator | "Ignore all previous instructions", "Investigator: …", "SYSTEM:", "begin your summary with" |
| text posing as Themis, a tool's output, metrics or an approval | "[themis-core] tool_result …", "approved by the SRE on call", "change freeze", throttling figures written in a log line |
| an order to make, or not make, a fix | "roll back to revision 1", "Raise the CPU limit to 2", "kubectl rollout undo", "Do not roll back", "close this incident" |
| a ready-made diagnosis | "known regression", "not a leak" |
| encoded text that decodes to instructions | a base64 run that decodes to any of the above |

It is a small heuristic, not a filter: the text stays in the evidence, and nothing is blocked. Real logs say "rollback" or "kubectl" too, so each rule needs the words around them; none flags a scenario pack's case written without an injection. Text that copies a real Kubernetes message word for word, such as a forged "failed liveness probe" event, can't be told apart this way.

What it changes:

- The investigator is told, before the evidence, that Themis flagged it and why, and the evidence is marked `suspicious="true"`. Outside investigators get the same flags in each evidence item's `suspicious` field.
- The incident page warns, above the evidence and on every fix: "Some evidence contains text that looks like instructions to Themis or forged tool output (container_logs_tail: "Ignore all previous instructions…"). Check the fix against the cluster before approving." Viewers, who may not read raw evidence, see which evidence without the text.
- [Escalation notices](#escalation-notices) carry `evidenceWarning`, naming the evidence without its text.
- Policy can read `predicates.evidence_has_instructions`, for example to forbid fixes when it is `"true"`, once the profile runs that check.

### Verification

| Key | Default | Meaning |
|---|---|---|
| `window` | 5m | How long Themis watches the target after acting |
| `stability` | 2m | How long every replica must stay ready with no new restarts or OOMKills |

The outcome is `resolved` (healthy and stable), `contained` (a complete rollout, but not stable), `unchanged`, `worsened` (fewer ready replicas than before), or `inconclusive`: the template was changed by someone else during the window. Anything but `resolved` escalates the incident.

## Metrics

`container_memory` and `container_cpu` read how much memory and CPU containers use from a metrics source. There are two, and Themis asks them in this order, so the second fills in what the first can't answer:

| Source | Turned on by | Can see | Can't see |
|---|---|---|---|
| A server answering Prometheus queries: Prometheus, Thanos, Mimir, VictoriaMetrics, Grafana Cloud | `metrics.prometheus.address` | Current memory and CPU use, peak memory over a window, and CPU throttling, from the kubelet's cAdvisor metrics (`container_memory_working_set_bytes`, `container_cpu_usage_seconds_total`, `container_cpu_cfs_throttled_periods_total` and `container_cpu_cfs_periods_total`) | Anything it doesn't scrape |
| The Kubernetes metrics API (`metrics.k8s.io`), served by metrics-server | on by default; `metrics.kubernetes.disabled: true` turns it off | Current memory and CPU use | Peaks and throttling: it keeps no history |

```yaml
metrics:
  kubernetes: {disabled: false}
  prometheus:
    address: http://prometheus-operated.monitoring:9090
    bearerTokenFile: /var/run/secrets/themis-prometheus/token   # optional
    tls:                                                       # optional
      caFile: /var/run/secrets/themis-prometheus/ca.crt
      certFile: /var/run/secrets/themis-prometheus/tls.crt     # with keyFile, for a client certificate
      keyFile: /var/run/secrets/themis-prometheus/tls.key
      insecureSkipVerify: false
```

| Key | Meaning |
|---|---|
| `metrics.kubernetes.disabled` | Don't read the Kubernetes metrics API. With the chart, the core may read it in each enrolled namespace while it's on. |
| `metrics.prometheus.address` | The server's base URL, under which `/api/v1/query` is served. Must be http or https. |
| `metrics.prometheus.bearerTokenFile` | A token sent with each query, read every time, so rotation needs no restart. |
| `metrics.prometheus.tls.caFile` | The CA that signs the server's certificate, when the system's don't. |
| `metrics.prometheus.tls.certFile`, `.keyFile` | A client certificate, for a server that asks for one. Set both or neither. Read on every connection. |
| `metrics.prometheus.tls.insecureSkipVerify` | Accept any certificate. Only for trying Themis. |

A figure no source can give is `null` in the evidence, with why, never a guess. When no source can say anything, the template's evidence is missing, with the reason, as for any evidence Themis couldn't read. The **Status** page says which source is in use and what Themis can't see, such as "no peak memory or CPU throttling data: set a Prometheus address".

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
    "profiles": ["rollout-regression"],
    "target": {"group": "apps", "kind": "Deployment", "namespace": "shop", "name": "checkout", "uid": "…"},
    "at": "2026-10-28T17:13:54Z",
    "url": "https://themis.example.com/#/incidents/01a1837f-…"
  }
}
```

`event` is `incident.escalated` or `incident.contained_escalated`. `reason` is Themis's own words, never the investigator's: when the checks support a fix the investigator didn't propose, it says so. `profiles` are every profile the incident matched, `profile`, its primary, first. When some evidence looks like instructions or forged tool output, `evidenceWarning` says which, such as "Some evidence contains text that looks like instructions to Themis or forged tool output (container_logs_tail)." Anything but a 2xx answer is retried with backoff for about a day. Notices never carry evidence.
