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

`model.baseURL` and `model.name` are required; the install fails without them.

### Model examples

| Endpoint | `model.baseURL` | `model.apiKeySecret` |
|---|---|---|
| OpenAI | `https://api.openai.com/v1` | a Secret with an `apiKey` key |
| OpenRouter | `https://openrouter.ai/api/v1` | a Secret with an `apiKey` key |
| Ollama in the cluster | `http://ollama.ollama.svc:11434/v1` | none |
| vLLM | `http://vllm.<namespace>.svc:8000/v1` | none, or a Secret if you set `--api-key` |

```bash
kubectl -n themis create secret generic model-key --from-literal=apiKey=sk-...
```

Container logs are classified `sensitive`, and with the default `model.ceiling: sensitive` they reach the model after redaction. To keep them from the endpoint, set `model.ceiling: internal`; investigations that need logs then fail rather than send them.

### Images

Images are named `<image.repository>/themis-{core,executor,intelligence}:<image.tag>`. Until images are published, build and load them yourself: `make images`, or `make kind-load` for a kind cluster.

## Let Themis act

```bash
helm upgrade themis deploy/helm/themis -n themis --reuse-values --set executor.enabled=true
```

This installs `themis-executor` with a Role in each enrolled namespace: `get` and `patch` on Deployments, and `get`/`list` on ReplicaSets. That is all it may do. In the default `guarded` mode every action still needs a human to approve its digest in the UI.

## Upgrade

`helm upgrade` doesn't update custom resource definitions. When a release changes them, apply them first: `kubectl apply -f deploy/helm/themis/crds`.

## Connect Alertmanager

Alertmanager must send alerts to `http://themis-core.<namespace>.svc:8080/v1/intake/alertmanager` with the intake token, the `intake` key of the `themis-tokens` Secret the chart generates. See [Connecting your Alertmanager](../../../docs/configuration.md#connecting-your-alertmanager).

## Open the UI

```bash
kubectl -n themis get secret themis-tokens -o jsonpath='{.data.admin}' | base64 -d; echo
kubectl -n themis port-forward svc/themis-core 8090:8080
```

Then open <http://127.0.0.1:8090> and sign in with that token.

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
| `image.repository` | `""` | Registry and path of the images |
| `image.tag` | `dev` | Image tag |
| `image.pullPolicy` | `IfNotPresent` | |
| `logLevel` | `info` | Least severe level every component logs: `debug`, `info`, `warn` or `error` |
| `telemetry.otlp.endpoint` | `""` | Where every component sends its traces over OTLP; empty, nothing is traced |
| `telemetry.otlp.protocol` | `grpc` | `grpc` or `http/protobuf` |
| `telemetry.otlp.egressCIDR` | `0.0.0.0/0` | Where the investigator may reach the collector, on ports 4317 and 4318 |
| `enrolledNamespaces` | `[shop]` | Namespaces Themis may read and, with the executor, write. Each gets its own Roles; Themis never creates RBAC at runtime. |
| `database.existingSecret` | `""` | A Secret with a `DATABASE_URL` key. Empty uses the bundled PostgreSQL. |
| `postgres.enabled` | `true` | A single PostgreSQL for trying Themis; not for production |
| `postgres.image`, `postgres.storage` | `postgres:17-alpine`, `1Gi` | |
| `model.baseURL` | **required** | An OpenAI-compatible endpoint |
| `model.name` | **required** | The model the endpoint serves |
| `model.apiKeySecret` | `""` | A Secret with an `apiKey` key, if the endpoint needs one |
| `model.ceiling` | `sensitive` | The most sensitive evidence the endpoint may receive: `public`, `internal` or `sensitive` |
| `model.egressCIDR` | `0.0.0.0/0` | Where the investigator may connect, for the NetworkPolicy |
| `model.tokenBudget` | `200000` | Tokens per investigation |
| `model.price` | `{}` | The endpoint's price in US dollars per million tokens, `inputUSDPerMTok` and `outputUSDPerMTok`, to report the cost of model requests |
| `executor.enabled` | `false` | Install the executor and its write Roles |
| `notifications.webhookSecret` | `""` | A Secret with a `url` key for escalation notices |
| `notifications.webhookToken` | `false` | Whether that Secret also has a `token` key, sent as a bearer token |
| `notifications.externalURL` | `""` | Where people reach the UI, for links in notices |
| `networkPolicy.enabled` | `true` | Limit the investigator to the core and the model, and the executor to the core |
| `profiles` | one profile | Incident classes, policy and verification, installed as `IncidentProfile` resources; see [Configuration](../../../docs/configuration.md#configuration-resources) |
| `themisConfig` | `{}` | Global settings, installed as the `ThemisConfig` resource when set |
| `resources.{core,executor,intelligence}` | small | Container resources |

## Uninstall

```bash
helm uninstall themis -n themis
```

The generated tokens are kept across upgrades. The bundled PostgreSQL's volume survives an uninstall; delete it, or the namespace, to remove the data too.
