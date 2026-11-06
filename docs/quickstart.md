# Quickstart

Try Themis in about 10 minutes, on a local kind cluster or on a cluster of your own. You install it, enroll one namespace, send it alerts, watch it find a bad rollout, and approve the rollback it suggests.

Themis runs in **guarded** mode: it never changes anything until a person approves the exact change.

Each step has a **kind** path and a **your cluster** path. The kind path uses the repository's `make` targets; for more detail on it, see [the demo](demo.md).

## What you need

- Docker, Helm and kubectl. The kind path also needs Go: kind runs through `go tool`.
- A clone of this repository, for the chart and the sample workload. The images are published, so there's nothing to build.
- A model: any OpenAI-compatible chat completions endpoint with JSON-schema structured output, such as OpenAI, OpenRouter, Ollama or vLLM. You can also set it after install, on the Setup page, or run Themis without one and suggest fixes yourself or through HolmesGPT.
- For your cluster: Kubernetes 1.35 or later, able to pull from `ghcr.io`, and an Alertmanager you can configure.

If the model needs a key, put it in a Secret first:

```bash
kubectl create namespace themis
kubectl -n themis create secret generic model-key --from-literal=apiKey=sk-...
```

On kind, run `make kind-up` before this, and add `--context kind-themis-dev` to both commands.

## 1. Install Themis

On kind, create the cluster first with `make kind-up` (skip it if you did above), and add `--context kind-themis-dev` to `kubectl` and `--kube-context kind-themis-dev` to `helm` below.

Create the namespace you'll enroll, then install the chart. Themis gets its access in each enrolled namespace at install, so the namespace must already exist:

```bash
kubectl create namespace shop
helm install themis deploy/helm/themis -n themis --create-namespace \
  --set enrolledNamespaces='{shop}' \
  --set executor.enabled=true \
  --set model.baseURL=https://api.openai.com/v1 \
  --set model.name=gpt-4.1 \
  --set model.apiKeySecret=model-key \
  --set model.schemaDialect=openai-strict \
  --wait
```

Leave out `model.apiKeySecret` if the model needs no key. `model.schemaDialect=openai-strict` is for OpenAI, which accepts Themis's reply schema in strict mode only in that form; leave it out for Ollama and vLLM. OpenAI's newer models also need `--set model.outputLimitField=max_completion_tokens --set model.temperature=default`. The images come from `ghcr.io/hzerrad`, signed: `main` is the latest build, and `--set image.tag=sha-<commit>` pins one.

To run Themis from your own checkout instead, `make themis-up MODEL_URL=... MODEL_NAME=...` builds the images, loads them into kind, and installs the chart.

| Value | What it does |
|---|---|
| `enrolledNamespaces` | The namespaces Themis may read and, with the executor, change. It gets a Role in each, and nothing else. |
| `executor.enabled` | Installs the only part that can change workloads. Without it, Themis only diagnoses and suggests. |
| `model.baseURL`, `model.name` | Your model. Leave them out to set the model on the **Setup** page instead, where its key is kept encrypted. With `model.enabled=false`, Themis installs no investigator, and each incident waits for a fix suggested by a person or a tool such as HolmesGPT. |
| `model.apiKeySecret` | The Secret with an `apiKey` key, if the model needs one |

The bundled PostgreSQL is for trying Themis. For anything longer, set `database.existingSecret` to a Secret with a `DATABASE_URL` key. All values are in the [chart README](../deploy/helm/themis/README.md#values).

## 2. Enroll a namespace

Themis only looks at enrolled namespaces. Both paths above enrolled `shop`. To use another, create it if it doesn't exist yet, change `enrolledNamespaces`, and upgrade:

```bash
helm upgrade themis deploy/helm/themis -n themis --reuse-values --set enrolledNamespaces='{payments}'
```

Put a workload there that you can break. The sample one is a Deployment named `checkout` in `shop`, with a revision that runs out of memory on every start:

**kind:**

```bash
make demo-up
```

**Your cluster:**

```bash
kubectl apply -f scripts/dev/demo/checkout.yaml
kubectl -n shop rollout status deployment/checkout
```

The chart's built-in profile handles this kind of incident: a Deployment whose new pods crash or run out of memory after a rollout, fixed by rolling back. It needs approval for every rollback.

## 3. Point Alertmanager at Themis

Alertmanager posts alerts to `http://themis-core.themis.svc:8080/v1/intake/alertmanager`, with the `intake` key of the `themis-tokens` Secret as a bearer token.

**kind:** this installs Prometheus and Alertmanager, already wired to Themis, with `KubePodCrashLooping` firing after 30 seconds instead of 15 minutes:

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
make monitoring-up
```

**Your cluster:** the **Setup** page (step 4) shows these steps filled in for your install, for kube-prometheus-stack or any other Alertmanager, ready to copy. By hand: copy the intake token into a Secret Alertmanager can mount. This assumes Alertmanager runs in `monitoring`:

```bash
kubectl -n themis get secret themis-tokens -o jsonpath='{.data.intake}' | base64 -d | \
  kubectl -n monitoring create secret generic themis-intake-token --from-file=token=/dev/stdin
```

Mount it in Alertmanager, at `/etc/alertmanager/themis` for example, then add a receiver and a route:

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

With the usual kube-prometheus rules, `KubePodCrashLooping` fires after 15 minutes. More in [Connecting your Alertmanager](configuration.md#connecting-your-alertmanager).

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

Themis starts once an admin account exists, and serves nothing else until then. Alerts sent before are refused, and Alertmanager sends them again later. The initial admin password only creates that account. Create your admin account, sign in with it, and choose your own password. The initial admin password then stops working. Opening Themis grants nothing by itself. Under GitOps, configure the admin account in the chart instead, and Themis creates it at startup. See [the chart README](../deploy/helm/themis/README.md#use-with-argo-cd-or-flux).

The first page is then **Setup**. Its steps are sign-in, where you can add your identity provider (step 6), the model if you didn't set it at install, and connecting Alertmanager. What Helm already set is shown as done. **Status** then checks every part and says how to fix what doesn't work. Then break the rollout, in another terminal:

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
| about 1 minute on kind | A checkout pod crash-loops, Alertmanager posts the alert, and an incident appears, **investigating** |
| 1 to 5 minutes more | Themis and the model read the evidence and suggest a rollback. The incident **needs your approval**. |

Open the incident. It shows what Themis found, what the model suggested, and the exact change Themis would make, read from the live cluster: the workload, the pods it affects, the policy rule that decided, and every field the rollback changes.

## 5. Approve the rollback

Click **Approve this change**. You approve exactly what you were shown: if the workload changes first, the approval no longer applies.

| After | You see |
|---|---|
| seconds | **approved**, then **applying the fix**: the executor checks the Deployment still matches, tries the rollback without saving it, then applies it |
| 2 to 5 minutes | **checking the fix**, then **fixed**: every pod ready, no new out-of-memory kills, and stable for the whole check |

The timeline holds the whole record, from the alert to the result. To try again, roll checkout back (`make demo-fix`, or `kubectl -n shop rollout undo deployment/checkout`) and break it again. [The demo](demo.md#things-to-try) has more to try, such as denying a fix or fixing it yourself first.

## 6. Optional: sign people in through your identity provider

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
