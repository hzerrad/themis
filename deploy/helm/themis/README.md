# Themis Helm chart

Installs `themis-core`, `themis-intelligence` and, if you enable it, `themis-executor`, with a bundled PostgreSQL for trying it out.

By default Themis is **read-only**: it investigates and proposes, and nothing it proposes can run. Enabling the executor adds the only component with write access to your workloads.

## Install

Themis brings no model. Point it at any OpenAI-compatible chat completions endpoint that supports JSON-schema structured output:

```bash
helm install themis deploy/helm/themis -n themis --create-namespace \
  --set enrolledNamespaces='{shop}' \
  --set model.baseURL=https://api.openai.com/v1 \
  --set model.name=gpt-4.1 \
  --set model.apiKeySecret=model-key
```

`model.baseURL` and `model.name` can also be left out and set on the **Setup** page after install. With `model.enabled=false`, no investigator is installed, and each incident waits for a fix suggested by a person or a tool such as HolmesGPT.

### Model examples

| Endpoint | `model.baseURL` | `model.apiKeySecret` |
|---|---|---|
| OpenAI | `https://api.openai.com/v1` | a Secret with an `apiKey` key |
| OpenRouter | `https://openrouter.ai/api/v1` | a Secret with an `apiKey` key |
| Ollama in the cluster | `http://ollama.ollama.svc:11434/v1` | none |
| vLLM | `http://vllm.<namespace>.svc:8000/v1` | none, or a Secret if you set `--api-key` |
| Anthropic, with `model.provider=anthropic` | none | a Secret with an `apiKey` key |

```bash
kubectl -n themis create secret generic model-key --from-literal=apiKey=sk-...
```

Container logs are classified `sensitive`, and with the default `model.ceiling: sensitive` they reach the model after redaction. To keep them from the endpoint, set `model.ceiling: internal`; investigations that need logs then fail rather than send them.

### Images

Images are named `<image.repository>/themis-{core,executor,intelligence}:<image.tag>`. By default they come from `ghcr.io/hzerrad`, published for linux/amd64 and linux/arm64 on every merge to main, and signed: `main` is the latest, and `sha-<commit>` pins an exact build. To build your own, run `make images`, or `make kind-load` for a kind cluster, then set `image.repository` and `image.tag`.

## Let Themis act

```bash
helm upgrade themis deploy/helm/themis -n themis --reuse-values --set executor.enabled=true
```

This installs `themis-executor` with a Role in each enrolled namespace: `get` and `patch` on Deployments, and `get`/`list` on ReplicaSets. That is all it may do. In the default `guarded` mode every action still needs a human to approve its digest in the UI.

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

Themis reads the active alerts from Alertmanager's API every 15 seconds. When Alertmanager runs in the cluster, set nothing: at install, the chart finds its Service with Helm's `lookup`, as prometheus-community's prometheus chart and kube-prometheus-stack install it, and `helm install` prints the address it reads. The lookup uses your access, and Themis gets no permission to list Services. Argo CD and Flux don't run `lookup`: set `alerting.alertmanager.address`, or the address on the **Setup** page.

```bash
helm upgrade themis deploy/helm/themis -n themis --reuse-values \
  --set alerting.alertmanager.address=http://alertmanager-operated.monitoring:9093 \
  --set 'alerting.alertmanager.matchers={cluster="prod-eu"}'   # for an Alertmanager several clusters share
```

An Alertmanager that needs a token, basic auth or its own CA gets them with its address, in `alerting.alertmanager`. To have Alertmanager send alerts to Themis's webhook instead, set `alerting.alertmanager.enabled=false`: the **Setup** page then shows the steps for your install. Alertmanager sends to `http://themis-core.<namespace>.svc:8080/v1/intake/alertmanager` with the intake token, the `intake` key of the `themis-tokens` Secret the chart generates (or of `tokens.existingSecret`). See [Connecting your Alertmanager](../../../docs/configuration.md#connecting-your-alertmanager).

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
helm upgrade themis deploy/helm/themis -n themis --reuse-values -f oidc.yaml
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
helm upgrade themis deploy/helm/themis -n themis --reuse-values \
  --set notifications.webhookSecret=themis-webhook \
  --set notifications.webhookToken=true \
  --set notifications.externalURL=https://themis.example.com
```

Every escalation is POSTed as JSON; the format is in [Escalation notices](../../../docs/configuration.md#escalation-notices).

## Values

| Value | Default | Meaning |
|---|---|---|
| `image.repository` | `ghcr.io/hzerrad` | Registry and path of the images |
| `image.tag` | `main` | Image tag: `main`, or `sha-<commit>` to pin a build |
| `image.pullPolicy` | `Always` | `main` moves, so it's pulled every time; use `IfNotPresent` with a pinned tag |
| `initialAdmin.existingSecret` | `""` | A Secret with a `password` key, the initial admin password, for GitOps (see above). Empty, the chart generates `<release>-initial-admin` |
| `initialAdmin.account.username` | `""` | An admin account Themis creates at startup while it has none. Set, no initial admin password exists |
| `initialAdmin.account.existingSecret` | `""` | Required with the username. A Secret with the password's hash or the password itself (see above) |
| `initialAdmin.account.passwordHashKey`, `.passwordKey` | `password-hash`, `password` | The keys of that Secret. A hash, bcrypt or argon2id, wins when both exist |
| `tokens.existingSecret` | `""` | A Secret with Themis's tokens and settings key, for GitOps; see above. Empty, the chart generates one |
| `logLevel` | `info` | Least severe level every component logs: `debug`, `info`, `warn` or `error` |
| `telemetry.otlp.endpoint` | `""` | Where every component sends its traces over OTLP; empty, nothing is traced |
| `telemetry.otlp.protocol` | `grpc` | `grpc` or `http/protobuf` |
| `telemetry.otlp.egressCIDR` | `0.0.0.0/0` | Where the investigator may reach the collector, on ports 4317 and 4318 |
| `enrolledNamespaces` | `[shop]` | Namespaces Themis may read and, with the executor, write. Each gets its own Roles; Themis never creates RBAC at runtime. |
| `alerting.alertmanager.enabled` | `true` | Read the active alerts from Alertmanager's API. Off, Alertmanager must send them to Themis's webhook |
| `alerting.alertmanager.address` | `""` | Alertmanager's base URL. Empty: the chart looks for one in the cluster at install, or the Setup page sets it |
| `alerting.alertmanager.tokenSecret` | `""` | A Secret with a `token` key, sent as a bearer token. Needs `address` |
| `alerting.alertmanager.basicAuth.username`, `.passwordSecret` | `""` | Basic auth: a username, and a Secret with a `password` key. Needs `address` |
| `alerting.alertmanager.tls.caSecret` | `""` | A Secret with a `ca.crt` key: the CA that signs Alertmanager's certificate. Needs `address` |
| `alerting.alertmanager.tls.clientCertSecret` | `""` | A `kubernetes.io/tls` Secret, for an Alertmanager that asks for a client certificate. Needs `address` |
| `alerting.alertmanager.tls.insecureSkipVerify` | `false` | Accept any certificate. Only for trying Themis |
| `alerting.alertmanager.matchers` | `[]` | Matchers that narrow the alerts read, such as `cluster="prod-eu"` |
| `alerting.alertmanager.interval` | `15s` | How often Alertmanager is read |
| `database.existingSecret` | `""` | A Secret with a `DATABASE_URL` key. Empty uses the bundled PostgreSQL. |
| `postgres.enabled` | `true` | A single PostgreSQL for trying Themis; not for production |
| `postgres.image`, `postgres.storage` | `postgres:17-alpine`, `1Gi` | |
| `model.enabled` | `true` | Install the investigator. Off, incidents wait for a suggested fix |
| `model.provider` | `""` (`openai`) | `openai` for an OpenAI-compatible endpoint, or `anthropic` for Claude models through Anthropic's API, which needs no `baseURL`. Empty with no `baseURL`, it's chosen on the Setup page |
| `model.baseURL` | `""` | An OpenAI-compatible endpoint. Empty, it's set on the Setup page |
| `model.name` | `""` | The model the endpoint serves. Empty, it's set on the Setup page |
| `model.apiKeySecret` | `""` | A Secret with an `apiKey` key, if the endpoint needs one |
| `model.ceiling` | `sensitive` | The most sensitive evidence the endpoint may receive: `public`, `internal` or `sensitive` |
| `model.egressCIDR` | `0.0.0.0/0` | Where the investigator may connect, for the NetworkPolicy |
| `model.tokenBudget` | `200000` | Tokens per incident, shared by its investigations when a person has it investigated again |
| `model.reasoningEffort` | `""` | How hard a reasoning model thinks, for example `none` or `low`; for `anthropic`, `low`, `medium`, `high`, `xhigh` or `max`. Empty leaves the provider's default |
| `model.maxOutputTokens` | `""` (`4096`) | Each reply's output limit. Raise it for a model that reasons, or its replies are cut off |
| `model.strictOutput` | `true` | Ask the provider to follow the reply schema exactly. Replies are still checked either way. Without `baseURL`, the Setup page's reply format sets it |
| `model.schemaDialect` | `""` (`as-is`) | How the reply schema is sent: `as-is`, as Ollama and vLLM take it, or `openai-strict`, the form OpenAI's strict mode accepts. Empty with no `baseURL`, the Setup page's reply format sets it. OpenAI needs `openai-strict` |
| `model.outputLimitField` | `""` (`max_tokens`) | The field the output limit is sent in, `max_tokens` or `max_completion_tokens`. OpenAI's newer models need `max_completion_tokens` |
| `model.temperature` | `""` (`0`) | The temperature sent: a number, or `default` to send none and leave the model's own, as OpenAI's newer models require |
| `model.price` | `{}` | The endpoint's price in US dollars per million tokens, `inputUSDPerMTok` and `outputUSDPerMTok`, to report the cost of model requests |
| `executor.enabled` | `false` | Install the executor and its write Roles |
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
| `networkPolicy.enabled` | `true` | Limit the investigator to the core and the model, and the executor to the core |
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
| `resources.{core,executor,intelligence}` | small | Container resources |
| `crds.upgrade` | `true` | Update the CRDs before each upgrade, with a Job |
| `crds.image` | `registry.k8s.io/kubectl:v1.37.2` | The image that Job runs |

## Uninstall

```bash
helm uninstall themis -n themis
```

The generated tokens are kept across upgrades. The bundled PostgreSQL's volume survives an uninstall; delete it, or the namespace, to remove the data too.

The CRDs stay too, and so do the `ThemisConfig` and `IncidentProfile` resources you created yourself. The ones the chart made from `profiles` and `themisConfig` are deleted. To remove the CRDs and every resource left: `kubectl delete crd themisconfigs.themis.dev incidentprofiles.themis.dev`.
