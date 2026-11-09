# Themis Helm chart

Installs `themis-core` and, when you turn them on, `themis-intelligence` and `themis-executor`, with a bundled PostgreSQL for trying it out.

By default Themis is **read-only**: it collects evidence and runs its checks, sends no evidence to a model and changes no workload. Each incident waits for a fix suggested by a person or a tool such as HolmesGPT, and a fix Themis accepts is only recommended, with the kubectl commands that make it. The investigator and fixes are turned on separately:

| Step | Values | Evidence sent outside the cluster | Changes to workloads |
|---|---|---|---|
| Read-only (default) | None | None | None |
| Investigator | `investigator.enabled=true`, and the model in `investigator.model` | To the model. A model in the cluster keeps it there | None |
| Fixes | `executor.enabled=true`, and the namespaces it may change in `executor.namespaces` | None, unless the investigator is on | Each fix after a person approves it, only in those namespaces. Putting back one that made things worse needs no approval by default |

## Install

```bash
kubectl create namespace shop
helm install themis oci://ghcr.io/hzerrad/charts/themis --version 0.1.0-alpha2 \
  -n themis --create-namespace \
  --set enrolledNamespaces='{shop}'
```

Themis watches only the namespaces in `enrolledNamespaces`. Each must exist before the install: Themis gets its Roles there then. The commands below upgrade the same published chart. From a checkout, use `deploy/helm/themis` in place of the chart and its version.

### Images

Images are named `<image.repository>/themis-{core,executor,intelligence}:<image.tag>`. They come from `ghcr.io/hzerrad`, published for linux/amd64 and linux/arm64, and signed. A published chart pins the images of its own version, such as `v0.1.0-alpha2`. The chart in the repository uses `main`, the build of every merge to main, and `sha-<commit>` pins an exact build. To build your own, run `make images`, or `make kind-load` for a kind cluster, then set `image.repository` and `image.tag`.

## Turn on the investigator

Themis brings no model. Point the investigator at any OpenAI-compatible chat completions endpoint that supports JSON-schema structured output, or at Claude through Anthropic's API:

```bash
helm upgrade themis oci://ghcr.io/hzerrad/charts/themis --version 0.1.0-alpha2 -n themis --reuse-values \
  --set investigator.enabled=true \
  --set investigator.model.baseURL=https://api.openai.com/v1 \
  --set investigator.model.name=gpt-4.1 \
  --set investigator.model.apiKeySecret=model-key \
  --set investigator.model.schemaDialect=openai-strict
```

`investigator.model.baseURL` and `investigator.model.name` can also be left out and set on the **Setup** page.

### Model examples

| Endpoint | `investigator.model.baseURL` | `investigator.model.apiKeySecret` |
|---|---|---|
| OpenAI | `https://api.openai.com/v1` | a Secret with an `apiKey` key |
| OpenRouter | `https://openrouter.ai/api/v1` | a Secret with an `apiKey` key |
| Ollama in the cluster | `http://ollama.ollama.svc:11434/v1` | none |
| vLLM | `http://vllm.<namespace>.svc:8000/v1` | none, or a Secret if you set `--api-key` |
| Anthropic, with `investigator.model.provider=anthropic` | none | a Secret with an `apiKey` key |

```bash
kubectl -n themis create secret generic model-key --from-literal=apiKey=sk-...
```

Container logs are classified `sensitive`, and with the default `investigator.model.ceiling: sensitive` they reach the model after redaction. To keep them from the endpoint, set `investigator.model.ceiling: internal`; investigations that need logs then fail rather than send them.

### What the investigator may reach

Its NetworkPolicy lets it reach the core, DNS and the model: any address in `investigator.egressCIDR`, every address by default, only on the model's port. That port is the one in `investigator.model.baseURL`, 80 for `http://` without one, and 443 otherwise, as for Anthropic's API or an address set on the Setup page. Set `investigator.egressCIDR` to the model's addresses where you can.

Set `investigator.egressPorts`, such as `[11434]`, for a model set on the Setup page at another port, or behind a Service whose port differs from its pods': the NetworkPolicy matches the pods' port. A model in the cluster is matched by its pods' IPs, which some network plugins, such as Cilium, never match to a CIDR. There, add a NetworkPolicy that lets the investigator's pods reach the model's.

## Turn on fixes

```bash
helm upgrade themis oci://ghcr.io/hzerrad/charts/themis --version 0.1.0-alpha2 -n themis --reuse-values \
  --set executor.enabled=true --set executor.namespaces='{shop}'
```

This installs `themis-executor` with a Role in each namespace `executor.namespaces` lists: `get` and `patch` on Deployments, and `get`/`list` on ReplicaSets. That is all it may do. Each must also be in `enrolledNamespaces`, and the chart refuses the executor with none listed. In the default `guarded` mode every fix still needs a person to approve its digest in the UI. Putting back what Themis changed, after a fix that made things worse, needs no approval unless a policy rule asks for one.

To limit which fixes it applies, list them in `executor.fixes`, such as `--set executor.fixes='{deployment.rollback-to-revision}'`. The executor checks the list itself before every write, and refuses any other fix, even one approved. Empty, it applies every fix the scenario packs offer. Themis reads both lists too: a fix outside them is only recommended, never put to a person to approve, as [recommendations](../../../docs/configuration.md#recommendations) say.

## Upgrade

`helm install` creates the `ThemisConfig` and `IncidentProfile` CRDs. Before each `helm upgrade`, a short Job runs `kubectl apply` on them, using the `registry.k8s.io/kubectl` image and a ClusterRole limited to those two CRDs. To manage the CRDs yourself, set `crds.upgrade=false`.

## Use with Argo CD or Flux

The chart generates Themis's tokens, the key that encrypts secret settings and the initial admin password once, and keeps them across upgrades with Helm's `lookup`. GitOps tools don't run `lookup`, so every sync would make new ones, and a new settings key makes saved secrets, such as a model key set on the Setup page, unreadable. Create the Secrets yourself instead, outside the synced manifests or with your secrets tool, and name them.

Under GitOps, configure the admin account too, so Themis starts with it and needs no initial admin password. Put the password's hash in a Secret. Values are kept in Git and in Helm's release history, so the chart refuses a password there. Use a bcrypt hash, as `htpasswd` makes it, or argon2id in the PHC string format:

```bash
kubectl -n themis create secret generic themis-tokens-gitops \
  $(for k in intake investigator admin settings executor; do printf -- "--from-literal=%s=%s " $k "$(openssl rand -hex 32)"; done)
read -rs PASSWORD   # at least 12 characters
kubectl -n themis create secret generic themis-admin \
  --from-literal=password-hash="$(htpasswd -nbBC 12 "" "$PASSWORD" | tr -d ':\n')"
```

```yaml
tokens:
  existingSecret: themis-tokens-gitops
initialAdmin:
  account:
    username: root
    existingSecret: themis-admin
```

At startup, while no admin account exists, Themis creates `root` with that password, recorded as made by the Helm chart. It never changes the account after. Change its password in Themis, as for any account. A Secret tool that can't hash, such as External Secrets or Sealed Secrets holding the password itself, sets a `password` key instead. Themis hashes it and never logs it. `initialAdmin.account.passwordHashKey` and `.passwordKey` name other keys. The Secret is read only while the account must be created, and Themis refuses to start if it is missing or unusable then.

To keep the initial admin password instead, create its Secret and set `initialAdmin.existingSecret`:

```bash
kubectl -n themis create secret generic themis-initial-admin-gitops --from-literal=password="$(openssl rand -hex 24)"
```

## Connect Alertmanager

Themis starts in poll mode: it reads the active alerts from Alertmanager's API every 15 seconds. When Alertmanager runs in the cluster, set nothing: at install, the chart finds its Service with Helm's `lookup`, as prometheus-community's prometheus chart and kube-prometheus-stack install it, and `helm install` prints the address it reads. The lookup uses your access, and Themis gets no permission to list Services. Argo CD and Flux don't run `lookup`: set `alerting.alertmanager.address`, or the address on the **Setup** page.

```bash
helm upgrade themis oci://ghcr.io/hzerrad/charts/themis --version 0.1.0-alpha2 -n themis --reuse-values \
  --set alerting.alertmanager.address=http://alertmanager-operated.monitoring:9093 \
  --set 'alerting.alertmanager.matchers={cluster="prod-eu"}'   # for an Alertmanager several clusters share
```

An Alertmanager that needs a token, basic auth or its own CA gets them with its address, in `alerting.alertmanager`.

The Setup page and the Status page say which of the alerts Themis answers have a rule in Prometheus: the one at `metrics.prometheus.address`, or the one the chart finds at install. kube-prometheus-stack lacks `KubePodOOMKilled`. With the Prometheus Operator, `--set alerting.rules.install=true` installs a PrometheusRule with the rules the usual stacks lack, labelled as the first `Prometheus` resource found selects rules, or with `alerting.rules.labels`. `alerting.rules.standard=true` adds the rest, for a Prometheus without kube-prometheus-stack's rules.

In push mode, Alertmanager sends alerts to Themis's webhook instead, and Themis reads nothing. An admin switches modes on the **Setup** page, which then shows the steps for your install. `alerting.mode=push` or `alerting.mode=poll` sets the mode in Helm instead, and the Setup page can't switch it. With the Prometheus Operator, `alerting.webhook.alertmanagerConfig.enabled=true` makes an AlertmanagerConfig in each enrolled namespace, which the operator limits to that namespace's alerts, with a Secret there holding the intake token. Under GitOps, manage that Secret yourself and name it in `alerting.webhook.tokenSecret`. Alertmanager sends to `http://themis-core.<namespace>.svc:8080/v1/intake/alertmanager` with the intake token, the `intake` key of the `themis-tokens` Secret the chart generates (or of `tokens.existingSecret`). See [Connecting your Alertmanager](../../../docs/configuration.md#connecting-your-alertmanager).

## Open the UI

```bash
kubectl -n themis port-forward deploy/themis-core 8090:8080
```

Then open <http://localhost:8090>. Themis starts once an admin account exists, and serves nothing else until then. It reads no alerts before. Sign in with the initial admin password to create the first admin account. The chart generates it once, in the `themis-initial-admin` Secret, and keeps it across upgrades:

```bash
kubectl -n themis get secret themis-initial-admin -o jsonpath='{.data.password}' | base64 -d
```

Then create the admin account with a password of your own. Creating it signs you in, and the initial admin password stops working. Opening Themis grants nothing by itself. To have Themis create the admin account at startup instead, see [Use with Argo CD or Flux](#use-with-argo-cd-or-flux).

## Sign people in

With an OIDC provider, people sign in as themselves, and each approval records who gave it. Any provider works: Dex, Google, Okta, Keycloak, Entra ID and others.

Register Themis with the provider as a web app. Its redirect URL is Themis's `/auth/callback` as the browser reaches it, for example `https://themis.example.com/auth/callback`. Put the client secret in a Secret, then set the values:

```bash
kubectl -n themis create secret generic themis-oidc --from-literal=clientSecret=...
helm upgrade themis oci://ghcr.io/hzerrad/charts/themis --version 0.1.0-alpha2 -n themis --reuse-values -f oidc.yaml
```

```yaml
# oidc.yaml
oidc:
  issuerURL: https://idp.example.com
  clientID: themis
  redirectURL: https://themis.example.com/auth/callback
  clientSecret: {existingSecret: themis-oidc}
access:
  - {role: admin, users: [alice@example.com]}
  - {role: approver, groups: [sre]}
  - {role: viewer, groups: [engineering]}
```

Someone who signs in but matches no entry in `access` sees nothing until an admin adds them. The admin token keeps working, as a way in if the provider is down. Roles are in [Configuration](../../../docs/configuration.md#people-and-roles).

## Escalation notices

```bash
kubectl -n themis create secret generic themis-webhook --from-literal=url=https://hooks.example.com/... --from-literal=token=...
helm upgrade themis oci://ghcr.io/hzerrad/charts/themis --version 0.1.0-alpha2 -n themis --reuse-values \
  --set notifications.webhookSecret=themis-webhook \
  --set notifications.webhookToken=true \
  --set notifications.externalURL=https://themis.example.com
```

Every escalation is POSTed as JSON; the format is in [Escalation notices](../../../docs/configuration.md#escalation-notices).

## Values

| Value | Default | Meaning |
|---|---|---|
| `image.repository` | `ghcr.io/hzerrad` | Registry and path of the images |
| `image.tag` | `main`, or a published chart's own version | Image tag: `main`, or `sha-<commit>` to pin a build |
| `image.pullPolicy` | `Always`, or `IfNotPresent` in a published chart | `main` moves, so it's pulled every time; use `IfNotPresent` with a pinned tag |
| `initialAdmin.existingSecret` | `""` | A Secret with a `password` key, the initial admin password, for GitOps (see above). Empty, the chart generates `<release>-initial-admin` |
| `initialAdmin.account.username` | `""` | An admin account Themis creates at startup while it has none. Set, no initial admin password exists |
| `initialAdmin.account.existingSecret` | `""` | Required with the username. A Secret with the password's hash or the password itself (see above) |
| `initialAdmin.account.passwordHashKey`, `.passwordKey` | `password-hash`, `password` | The keys of that Secret. A hash, bcrypt or argon2id, wins when both exist |
| `tokens.existingSecret` | `""` | A Secret with Themis's tokens and settings key, for GitOps; see above. Empty, the chart generates one |
| `logLevel` | `info` | Least severe level every component logs: `debug`, `info`, `warn` or `error` |
| `telemetry.otlp.endpoint` | `""` | Where every component sends its traces over OTLP; empty, nothing is traced |
| `telemetry.otlp.protocol` | `grpc` | `grpc` or `http/protobuf` |
| `telemetry.otlp.egressCIDR` | `0.0.0.0/0` | Where the investigator may reach the collector, on ports 4317 and 4318 |
| `enrolledNamespaces` | `[shop]` | Namespaces Themis watches. Each gets its own Roles; Themis never creates RBAC at runtime. The executor changes only those `executor.namespaces` lists. |
| `alerting.mode` | `""` | `poll`: Themis reads the active alerts from Alertmanager's API. `push`: Alertmanager sends them to Themis's webhook. Empty starts in poll mode, and the Setup page can switch it. `alerting.alertmanager.enabled=false`, from earlier charts, means `push` |
| `alerting.alertmanager.address` | `""` | Alertmanager's base URL, read in poll mode. Empty: the chart looks for one in the cluster at install, or the Setup page sets it |
| `alerting.alertmanager.tokenSecret` | `""` | A Secret with a `token` key, sent as a bearer token. Needs `address` |
| `alerting.alertmanager.basicAuth.username`, `.passwordSecret` | `""` | Basic auth: a username, and a Secret with a `password` key. Needs `address` |
| `alerting.alertmanager.tls.caSecret` | `""` | A Secret with a `ca.crt` key: the CA that signs Alertmanager's certificate. Needs `address` |
| `alerting.alertmanager.tls.clientCertSecret` | `""` | A `kubernetes.io/tls` Secret, for an Alertmanager that asks for a client certificate. Needs `address` |
| `alerting.alertmanager.tls.insecureSkipVerify` | `false` | Accept any certificate. Only for trying Themis |
| `alerting.alertmanager.matchers` | `[]` | Matchers that narrow the alerts read, such as `cluster="prod-eu"` |
| `alerting.alertmanager.interval` | `15s` | How often Alertmanager is read |
| `alerting.rules.install` | `false` | Install a PrometheusRule with the alerting rules the usual stacks lack, such as `KubePodOOMKilled`, when the cluster serves PrometheusRule |
| `alerting.rules.standard` | `false` | Also the rules kube-prometheus-stack has, for a Prometheus without them |
| `alerting.webhook.alertmanagerConfig.enabled` | `false` | With the Prometheus Operator, an AlertmanagerConfig in each enrolled namespace that sends its alerts to Themis's webhook, and a Secret there with the intake token. Anyone who can read Secrets there can then send alerts to Themis |
| `alerting.webhook.alertmanagerConfig.labels` | `{}` | Labels your Alertmanager selects AlertmanagerConfigs by, if any |
| `alerting.webhook.tokenSecret` | `""` | A Secret you manage, with a `token` key holding the intake token, in Alertmanager's namespace and each enrolled one. Required for `alertmanagerConfig` with `tokens.existingSecret` |
| `alerting.rules.labels` | `{}` | The PrometheusRule's labels, which your Prometheus selects rules by. Empty: those of the first `Prometheus` resource found at install |
| `database.existingSecret` | `""` | A Secret with a `DATABASE_URL` key. Empty uses the bundled PostgreSQL. |
| `postgres.enabled` | `true` | A single PostgreSQL for trying Themis; not for production |
| `postgres.image`, `postgres.storage` | `postgres:17-alpine`, `1Gi` | |
| `investigator.enabled` | `false` | Install the investigator. Off, each incident waits for a suggested fix |
| `investigator.tokenBudget` | `200000` | Tokens per incident, shared by its investigations when a person has it investigated again |
| `investigator.egressCIDR` | `0.0.0.0/0` | Where the investigator may reach the model, for the NetworkPolicy |
| `investigator.egressPorts` | `[]` | The ports it may reach the model on, such as `[11434]`. Empty: the port in `investigator.model.baseURL`, 80 for `http://` without one, and 443 otherwise. See [What the investigator may reach](#what-the-investigator-may-reach) |
| `investigator.resources` | small | The investigator's container resources |
| `investigator.model.provider` | `""` (`openai`) | `openai` for an OpenAI-compatible endpoint, or `anthropic` for Claude models through Anthropic's API, which needs no `baseURL`. Empty with no `baseURL`, it's chosen on the Setup page |
| `investigator.model.baseURL` | `""` | An OpenAI-compatible endpoint. Empty, it's set on the Setup page |
| `investigator.model.name` | `""` | The model the endpoint serves. Empty, it's set on the Setup page |
| `investigator.model.apiKeySecret` | `""` | A Secret with an `apiKey` key, if the endpoint needs one |
| `investigator.model.ceiling` | `sensitive` | The most sensitive evidence the endpoint may receive: `public`, `internal` or `sensitive` |
| `investigator.model.reasoningEffort` | `""` | How hard a reasoning model thinks, for example `none` or `low`; for `anthropic`, `low`, `medium`, `high`, `xhigh` or `max`. Empty leaves the provider's default |
| `investigator.model.maxOutputTokens` | `""` (`4096`) | Each reply's output limit. Raise it for a model that reasons, or its replies are cut off |
| `investigator.model.strictOutput` | `true` | Ask the provider to follow the reply schema exactly. Replies are still checked either way. Without `baseURL`, the Setup page's reply format sets it |
| `investigator.model.schemaDialect` | `""` (`as-is`) | How the reply schema is sent: `as-is`, as Ollama and vLLM take it, or `openai-strict`, the form OpenAI's strict mode accepts. Empty with no `baseURL`, the Setup page's reply format sets it. OpenAI needs `openai-strict` |
| `investigator.model.outputLimitField` | `""` (`max_tokens`) | The field the output limit is sent in, `max_tokens` or `max_completion_tokens`. OpenAI's newer models need `max_completion_tokens` |
| `investigator.model.temperature` | `""` (`0`) | The temperature sent: a number, or `default` to send none and leave the model's own, as OpenAI's newer models require |
| `investigator.model.price` | `{}` | The endpoint's price in US dollars per million tokens, `inputUSDPerMTok` and `outputUSDPerMTok`, to report the cost of model requests |
| `executor.enabled` | `false` | Install the executor and its write Roles |
| `executor.namespaces` | `[]` | Required with the executor: the namespaces it may change, each also in `enrolledNamespaces`. Its Roles are only there |
| `executor.fixes` | `[]` | The fixes the executor may apply, such as `deployment.rollback-to-revision`. Empty: every fix the scenario packs offer |
| `notifications.webhookSecret` | `""` | A Secret with a `url` key for escalation notices |
| `notifications.webhookToken` | `false` | Whether that Secret also has a `token` key, sent as a bearer token |
| `notifications.externalURL` | `""` | Where people reach the UI, for links in notices |
| `oidc.issuerURL` | `""` | The OIDC provider's issuer. Empty: no one signs in through OIDC |
| `oidc.clientID` | `""` | Required with `oidc.issuerURL` |
| `oidc.redirectURL` | `""` | Required with `oidc.issuerURL`: Themis's `/auth/callback` as the browser reaches it |
| `oidc.clientSecret.existingSecret` | `""` | Required with `oidc.issuerURL`: a Secret holding the client secret |
| `oidc.clientSecret.key` | `clientSecret` | The key in that Secret |
| `oidc.usernameClaim` | `""` (`email`) | The ID token claim used as the username |
| `oidc.groupsClaim` | `""` (`groups`) | The ID token claim listing the person's groups |
| `oidc.sessionTTL` | `""` (`8h`) | How long a sign-in lasts |
| `oidc.scopes` | `[]` (`email`, `profile`) | Scopes to ask for, besides `openid`. Add `groups` for Dex or Okta |
| `access` | `[]` | Roles for people who sign in, by `users` or `groups`; see [People and roles](../../../docs/configuration.md#people-and-roles) |
| `approvals.requireAnotherApprover` | `false` | Refuse the approval of a fix by the person who suggested it: someone else must approve it |
| `holmesgpt.enabled` | `false` | Install an adapter that puts each incident waiting for a suggestion to an existing HolmesGPT, which may suggest one of the fixes the incident offers. See [Fixes suggested from outside](../../../docs/configuration.md#fixes-suggested-from-outside) |
| `holmesgpt.url` | `""` | Required with `holmesgpt.enabled`: HolmesGPT's HTTP API, such as `http://holmes-holmes.holmes`. Version 0.25 or later |
| `holmesgpt.apiKeySecret` | `""` | A Secret with an `apiKey` key: HolmesGPT's `HOLMES_API_KEY` |
| `holmesgpt.model` | `""` | A key of HolmesGPT's `modelList`. Empty uses its default |
| `holmesgpt.perMinute` | `6` | How many suggestions a minute it may make |
| `holmesgpt.fixes` | `[]` | The fixes it may suggest, such as `deployment.rollback-to-revision`. Themis refuses any other from it. Empty: every fix the incident's profiles offer |
| `networkPolicy.enabled` | `true` | Limit the investigator to the core, DNS and the model's port, and the executor to the core |
| `metrics.kubernetes.enabled` | `true` | Read containers' current memory and CPU use from the Kubernetes metrics API (metrics-server), and let the core read it in each enrolled namespace |
| `metrics.prometheus.address` | `""` | A server answering Prometheus queries, for peak memory and CPU throttling too; see [Metrics](../../../docs/configuration.md#metrics). Empty: not used |
| `metrics.prometheus.tokenSecret` | `""` | A Secret with a `token` key, sent as a bearer token |
| `metrics.prometheus.tls.caSecret` | `""` | A Secret with a `ca.crt` key: the CA that signs the server's certificate |
| `metrics.prometheus.tls.clientCertSecret` | `""` | A `kubernetes.io/tls` Secret, for a server that asks for a client certificate |
| `metrics.prometheus.tls.insecureSkipVerify` | `false` | Accept any certificate. Only for trying Themis |
| `scenarios.disabled` | `[]` | [Scenario packs](../../../docs/scenarios.md) to turn off, by name; every pack is on otherwise. A name no pack has stops Themis from starting |
| `profiles` | `[]` | Incident classes of your own, installed as `IncidentProfile` resources. One named like a scenario pack's profile replaces it; see [Configuration](../../../docs/configuration.md#configuration-resources) |
| `themisConfig` | `{}` | Global settings, installed as the `ThemisConfig` resource when set |
| `networkPolicy.agentNamespaces` | `[]` | Namespaces whose pods may reach the core's gRPC API, for [agents that suggest fixes](../../../docs/configuration.md#fixes-suggested-from-outside) |
| `resources.{core,executor,holmesgpt}` | small | Container resources |
| `crds.upgrade` | `true` | Update the CRDs before each upgrade, with a Job |
| `crds.image` | `registry.k8s.io/kubectl:v1.37.2` | The image that Job runs |

## Uninstall

```bash
helm uninstall themis -n themis
```

The generated tokens are kept across upgrades. The bundled PostgreSQL's volume survives an uninstall; delete it, or the namespace, to remove the data too.

The CRDs stay too, and so do the `ThemisConfig` and `IncidentProfile` resources you created yourself. The ones the chart made from `profiles` and `themisConfig` are deleted. To remove the CRDs and every resource left: `kubectl delete crd themisconfigs.themis.dev incidentprofiles.themis.dev`.
