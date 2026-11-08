# Quickstart

Try Themis in about 10 minutes, on a local kind cluster or on a cluster of your own. You install it, enroll one namespace, send it alerts, watch it find a bad rollout, and approve the rollback it suggests.

Themis is turned on in steps, each with its own Helm values:

| Step | Helm values | Evidence sent outside the cluster | Changes to workloads |
|---|---|---|---|
| Read-only (default) | None | None | None |
| Investigator | `investigator.enabled=true`, and the model in `investigator.model` | To the model. A model in the cluster keeps it there | None |
| Fixes | `executor.enabled=true` | None, unless the investigator is on | Each one after a person approves it |

Each numbered step below has a **kind** path and a **your cluster** path. On kind, Themis is installed with the investigator and fixes on, so you see the whole loop. The kind path uses the repository's `make` targets; for more detail on it, see [the demo](demo.md). On your cluster, Themis starts read-only, and steps 5 and 6 add the investigator, then fixes.

Themis runs in **guarded** mode: it never changes anything until a person approves the exact change.

## What you need

- Docker, Helm and kubectl. The kind path also needs Go: kind runs through `go tool`.
- A clone of this repository, for the chart and the sample workload. The images are published for amd64 and arm64, so there's nothing to build.
- For the investigator, a model: any OpenAI-compatible chat completions endpoint with JSON-schema structured output, such as OpenAI, OpenRouter, Ollama or vLLM, or Claude through Anthropic's API. You can also set it on the Setup page once the investigator is on. Without one, you suggest fixes yourself or through HolmesGPT.
- For your cluster: Kubernetes 1.35 or later, able to pull from `ghcr.io`, and Prometheus with Alertmanager.

## 1. Install Themis

**kind:** create the cluster, then install Prometheus and Alertmanager, so the chart finds Alertmanager at install. `KubePodCrashLooping` fires after 30 seconds there, instead of 15 minutes:

```bash
make kind-up
make monitoring-up
```

If the model needs a key, put it in a Secret:

```bash
kubectl --context kind-themis-dev create namespace themis
kubectl --context kind-themis-dev -n themis create secret generic model-key --from-literal=apiKey=sk-...
```

Create the namespace you'll enroll, then install the chart with the investigator and the executor. Themis gets its access in each enrolled namespace at install, so the namespace must already exist. `metrics.prometheus.address` has Themis read the containers' memory and CPU figures, such as peaks and throttling, from the Prometheus `make monitoring-up` installed:

```bash
kubectl --context kind-themis-dev create namespace shop
helm --kube-context kind-themis-dev install themis deploy/helm/themis -n themis --create-namespace \
  --set enrolledNamespaces='{shop}' \
  --set metrics.prometheus.address=http://monitoring-prometheus-server.monitoring.svc \
  --set investigator.enabled=true \
  --set investigator.model.baseURL=https://api.openai.com/v1 \
  --set investigator.model.name=gpt-4.1 \
  --set investigator.model.apiKeySecret=model-key \
  --set investigator.model.schemaDialect=openai-strict \
  --set executor.enabled=true \
  --wait
```

For Claude, set the provider instead of an address. Anthropic's API has only one:

```bash
helm --kube-context kind-themis-dev install themis deploy/helm/themis -n themis --create-namespace \
  --set enrolledNamespaces='{shop}' \
  --set metrics.prometheus.address=http://monitoring-prometheus-server.monitoring.svc \
  --set investigator.enabled=true \
  --set investigator.model.provider=anthropic \
  --set investigator.model.name=claude-haiku-5-5 \
  --set investigator.model.apiKeySecret=model-key \
  --set executor.enabled=true \
  --wait
```

The model values are explained in [step 5](#5-add-the-investigator). To run Themis from your own checkout instead, `make themis-up MODEL_URL=... MODEL_NAME=... MODEL_KEY_SECRET=model-key` builds the images, loads them into kind, and installs the chart with the same values. For Claude, give `MODEL_PROVIDER=anthropic` instead of `MODEL_URL`.

**Your cluster:** create the namespace you'll enroll, then install the chart. It installs read-only:

```bash
kubectl create namespace shop
helm install themis deploy/helm/themis -n themis --create-namespace \
  --set enrolledNamespaces='{shop}' \
  --wait
```

| Value | What it does |
|---|---|
| `enrolledNamespaces` | The namespaces Themis may read and, with the executor, change. It gets a Role in each, and nothing else. |
| `investigator.enabled` | Installs the investigator, which sends the evidence to the model in `investigator.model`. Off, each incident waits for a fix suggested by a person or a tool such as HolmesGPT. |
| `executor.enabled` | Installs the only part that can change workloads. Off, Themis applies no fix. |

The images come from `ghcr.io/hzerrad`, signed: `main` is the latest build, and `--set image.tag=sha-<commit>` pins one. The bundled PostgreSQL is for trying Themis. For anything longer, set `database.existingSecret` to a Secret with a `DATABASE_URL` key. All values are in the [chart README](../deploy/helm/themis/README.md#values).

## 2. Enroll a namespace

Themis only looks at enrolled namespaces. Both paths above enrolled `shop`. To use another, create it if it doesn't exist yet, change `enrolledNamespaces`, and upgrade:

```bash
helm upgrade themis deploy/helm/themis -n themis --reuse-values --set enrolledNamespaces='{payments}'
```

Put a workload there that you can break. The sample one is a Deployment named `checkout` in `shop`, with a revision that runs out of memory on every start:

**kind:**

```bash
make demo-up   # also catalog and search, for the other scenario packs in the demo
```

**Your cluster:**

```bash
kubectl apply -f scripts/dev/demo/checkout.yaml
kubectl -n shop rollout status deployment/checkout
```

The chart's built-in profile handles this kind of incident: a Deployment whose new pods crash or run out of memory after a rollout, fixed by rolling back. It needs approval for every rollback.

## 3. Connect Alertmanager

Nothing to do when Alertmanager runs in the cluster. The chart found it at install, and Themis starts in poll mode: it reads Alertmanager's active alerts every 15 seconds. `helm install` printed the address it reads, and the **Setup** page shows it, with a **Test** button. **Status** then says "Poll mode. Last read 12 s ago."

When the chart found none, as under Argo CD or Flux, which don't let it look, or when Alertmanager runs elsewhere, set its address on the **Setup** page, or install with `--set alerting.alertmanager.address=http://alertmanager-operated.monitoring:9093`. For an Alertmanager several clusters share, add `--set 'alerting.alertmanager.matchers={cluster="prod-eu"}'` to read only this cluster's alerts.

With the usual kube-prometheus rules, `KubePodCrashLooping` fires after 15 minutes. kube-prometheus-stack has no `KubePodOOMKilled`: the Setup page says which of the alerts Themis answers your Prometheus lacks, with their rules to copy, and with the Prometheus Operator, `--set alerting.rules.install=true` installs them. To have Alertmanager send alerts to Themis instead, choose **Switch to push mode** on the Setup page, which then shows the steps. More in [Connecting your Alertmanager](configuration.md#connecting-your-alertmanager).

## 4. See an incident

Open the UI:

**kind:**

```bash
make ui   # serves the UI on http://localhost:8090
```

**Your cluster:**

```bash
kubectl -n themis port-forward deploy/themis-core 8090:8080
```

Open <http://localhost:8090> and sign in with the initial admin password. The chart generated it at install; on kind, add `--context kind-themis-dev`:

```bash
kubectl -n themis get secret themis-initial-admin -o jsonpath='{.data.password}' | base64 -d
```

Themis starts once an admin account exists, and serves nothing else until then. It reads no alerts before. The initial admin password only creates that account. Create your admin account with a password of your own. Creating it signs you in, and the initial admin password stops working. Opening Themis grants nothing by itself. Under GitOps, configure the admin account in the chart instead, and Themis creates it at startup. See [the chart README](../deploy/helm/themis/README.md#use-with-argo-cd-or-flux).

The first page is then **Setup**. Its steps are sign-in, where you can add your identity provider (step 7), the model when the investigator is on and you didn't set it at install, with **Test the connection** to check it, and how alerts reach Themis, in poll or push mode. What Helm already set is shown as done. **Status** then checks every part and says how to fix what doesn't work. A part that is off, such as the investigator or fixes, shows the Helm value that turns it on. Then break the rollout, in another terminal:

**kind:**

```bash
make demo-break
```

**Your cluster:**

```bash
kubectl -n shop set env deployment/checkout MODE=leak
```

| After | You see |
|---|---|
| about 1 minute on kind | A checkout pod crash-loops, Themis reads the alert from Alertmanager, and an incident appears |
| 1 to 5 minutes more, with the investigator | The incident is **Investigating** while Themis and the model read the evidence, then **Waiting for your approval** of the rollback they suggest |
| seconds more, read-only | Themis collects the evidence and runs its checks. The incident is **Waiting for a suggested fix** |

Open the incident. It states the facts Themis's checks found, each citing its evidence by number, then every piece of evidence, numbered, each opening to a view of its kind with its raw data. **Collect more evidence**, under the list, has Themis read more of it, such as another pod's logs. On a read-only install, the title says what fails, such as "checkout runs out of memory.", and you suggest the fix below the facts, with a preview of what Themis would do with it as you type. With the investigator, the page opens with the fix it asks you to approve, and adds below the facts the exact change Themis would make, read from the live cluster, with every field the rollback changes and the pods it restarts, and the checks for and against it, in one line that opens.

## 5. Add the investigator

On kind, the investigator is on already: go on to step 6.

On your cluster, if the model needs a key, put it in a Secret, then upgrade with the investigator and its model:

```bash
kubectl -n themis create secret generic model-key --from-literal=apiKey=sk-...
helm upgrade themis deploy/helm/themis -n themis --reuse-values \
  --set investigator.enabled=true \
  --set investigator.model.baseURL=https://api.openai.com/v1 \
  --set investigator.model.name=gpt-4.1 \
  --set investigator.model.apiKeySecret=model-key \
  --set investigator.model.schemaDialect=openai-strict \
  --wait
```

For Claude, set `investigator.model.provider=anthropic` and `investigator.model.name=claude-haiku-5-5`, and leave out the address and the schema dialect.

| Value | What it does |
|---|---|
| `investigator.model.provider` | `anthropic` for Claude through Anthropic's API, which needs no `investigator.model.baseURL`. Empty means an OpenAI-compatible endpoint. |
| `investigator.model.baseURL`, `investigator.model.name` | Your model. Leave them out to set the model on the **Setup** page instead, where its key is kept encrypted. |
| `investigator.model.apiKeySecret` | The Secret with an `apiKey` key, if the model needs one |

Leave out `investigator.model.apiKeySecret` if the model needs no key. `investigator.model.schemaDialect=openai-strict` is for OpenAI, which accepts Themis's reply schema in strict mode only in that form; leave it out for Ollama and vLLM. OpenAI's newer models also need `--set investigator.model.outputLimitField=max_completion_tokens --set investigator.model.temperature=default`.

Roll checkout back, then break it again:

```bash
kubectl -n shop rollout undo deployment/checkout
kubectl -n shop set env deployment/checkout MODE=leak
```

Within 1 to 5 minutes, the investigator reads the evidence and suggests a rollback, and Themis checks it. Themis applies it only once fixes are on.

## 6. Turn on fixes and approve the rollback

On kind, fixes are on already.

On your cluster, upgrade with the executor, the only part that can change workloads, then roll checkout back and break it again, as in step 5:

```bash
helm upgrade themis deploy/helm/themis -n themis --reuse-values --set executor.enabled=true --wait
```

Click **Approve the rollback**. You approve exactly what you were shown: if the workload changes first, the approval no longer applies.

| After | You see |
|---|---|
| seconds | **Applying**: the executor checks the Deployment still matches, tries the rollback without saving it, then applies it |
| 2 to 5 minutes | **Verifying**, then **Fixed**: every pod ready, no new out-of-memory kills, and stable for the whole check |

The timeline holds the whole record, from the alert to the result. To try again, roll checkout back (`make demo-fix`, or `kubectl -n shop rollout undo deployment/checkout`) and break it again. [The demo](demo.md#things-to-try) has more to try, such as denying a fix or fixing it yourself first.

## 7. Optional: sign people in through your identity provider

Themis accounts, on the **Accounts** page, are enough for a small team. With an OIDC provider, people sign in as themselves, each with a role, and an approval records who gave it. Any OIDC provider works, such as Dex, Google, Okta, Keycloak or Entra ID.

1. Register Themis with your provider as a web app. Set its redirect URL to Themis's `/auth/callback` as your browser reaches it. Through the port-forward above, that is `http://localhost:8090/auth/callback`; behind an ingress, something like `https://themis.example.com/auth/callback`.
2. Put the client secret in a Secret:

   ```bash
   kubectl -n themis create secret generic themis-oidc --from-literal=clientSecret=...
   ```

3. Write `oidc.yaml` with your provider's issuer, the client ID, and who gets which role:

   ```yaml
   oidc:
     issuerURL: https://idp.example.com
     clientID: themis
     redirectURL: http://localhost:8090/auth/callback
     clientSecret: {existingSecret: themis-oidc}
   access:
     - {role: admin, users: [you@example.com]}
     - {role: approver, groups: [sre]}
     - {role: viewer, groups: [engineering]}
   ```

4. Upgrade, and restart the port-forward once the core is back:

   ```bash
   helm upgrade themis deploy/helm/themis -n themis --reuse-values -f oidc.yaml --wait
   ```

   On kind, add `--kube-context kind-themis-dev`.

| Role | Can |
|---|---|
| `viewer` | See incidents, suggested fixes and their outcomes |
| `responder` | Also read evidence such as container logs |
| `approver` | Also suggest, approve and deny fixes |
| `admin` | Everything |

The admin account stays: Themis always needs one of its own, and the last can't be deleted. Someone who signs in but has no role sees nothing, and is told to ask a Themis admin. The break-glass token, in the `themis-tokens` Secret, still works for scripts and as a way in if the provider is down. The username is the `email` claim by default; set `oidc.usernameClaim` and `oidc.groupsClaim` if your provider uses others. For Dex or Okta, add `groups` to `oidc.scopes` to receive groups. More in [People and roles](configuration.md#people-and-roles).

## Clean up

**kind:**

```bash
make kind-down
```

**Your cluster:**

```bash
helm uninstall themis -n themis
kubectl delete -f scripts/dev/demo/checkout.yaml   # also deletes namespace shop
```

The bundled PostgreSQL's volume survives an uninstall: delete it, or the `themis` namespace, to remove the data too.
