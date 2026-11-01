# The bad-rollout demo

This walks through Themis's first incident class on a local kind cluster, with real Prometheus and Alertmanager. A `checkout` Deployment rolls out a revision whose container fills its memory and is OOMKilled on every start; the rollout gets stuck with the old pods still serving. Themis notices, investigates, proposes rolling back, and, once you approve, rolls back and checks that it worked.

It takes about 15 minutes, most of it waiting for images and the model.

## What you need

- Docker, Helm, kubectl and Go. kind runs through `go tool`, so it needs no install.
- A model: any OpenAI-compatible chat completions endpoint that supports JSON-schema structured output. Two common choices are below.

## 1. Create the cluster

```bash
make kind-up
```

## 2. Choose a model

Themis brings no model. Pick one the cluster can reach.

**A hosted endpoint.** Put the key in a Secret first:

```bash
kubectl --context kind-themis-dev create namespace themis
kubectl --context kind-themis-dev -n themis create secret generic model-key --from-literal=apiKey=sk-...
# then, in step 3: MODEL_URL=https://api.openai.com/v1 MODEL_NAME=gpt-4.1 MODEL_KEY_SECRET=model-key
```

**Ollama on your machine.** Pods reach your machine at the kind network's gateway. Bind Ollama to that address only, so it is not exposed on your network:

```bash
docker network inspect kind -f '{{range .IPAM.Config}}{{.Gateway}} {{end}}'   # for example 172.23.0.1
OLLAMA_HOST=172.23.0.1:11434 ollama serve                                     # in its own terminal
ollama pull qwen3:8b
# then, in step 3: MODEL_URL=http://172.23.0.1:11434/v1 MODEL_NAME=qwen3:8b
```

A host firewall may block the kind network from reaching that port. With `ufw`, allow just this interface and port: `sudo ufw allow in on <kind bridge> to 172.23.0.1 port 11434 proto tcp`, where `ip -4 -o addr show | grep 172.23.0.1` names the bridge.

Small local models work, but make more mistakes: a wrong target, or giving up early. Themis rejects those and records them, so you will see them under *Rejected attempts*. A larger model gets through more often on the first try.

## 3. Install Themis and monitoring

```bash
make themis-up MODEL_URL=... MODEL_NAME=...   # builds the images, loads them into kind, installs the chart with the executor
make monitoring-up                            # Prometheus, kube-state-metrics, Alertmanager wired to Themis
make demo-up                                  # the healthy checkout workload in namespace shop
```

`monitoring-up` uses a fast alert rule: `KubePodCrashLooping` fires after 30 seconds instead of the usual 15 minutes, so the demo moves at a watchable pace.

## 4. Open the UI

```bash
make ui   # prints the admin token and serves the UI on http://127.0.0.1:8090
```

Sign in with the token. The list is empty for now.

## 5. Break the rollout

```bash
make demo-break
```

What happens next, and roughly when:

| After | You see |
|---|---|
| ~1 min | A checkout pod crash-loops. Alertmanager posts the alert and an incident appears, **investigating**. |
| ~1–5 min | Themis and the model read the evidence, including the failing pod's logs, and suggest a fix. The incident **needs your approval**. |

Open the incident. From the top:

- **The summary** says what Themis found and what it expects from you.
- **What the model suggested** (left): the fix, why, and the evidence it cites. Themis doesn't trust it.
- **The change Themis would make** (right), read from the live cluster: the exact workload, how many pods it affects, which policy rule decided, and a change ID. *What changes* lists every field the rollback changes, here `env MODE: leak → healthy`. Values that look like secrets are hidden.
- **Suggestions Themis refused**, if any, such as one naming the wrong workload or citing evidence that doesn't exist.
- **Investigation**, **What Themis checked**, **Timeline** and **Evidence** are the work behind it: the possible causes the model weighed, the facts Themis checked itself, every step and who took it, and the evidence with secrets hidden.

## 6. Approve

Click **Approve this change**. You approve exactly the change you were shown: if the workload changes first, the approval no longer applies.

| After | You see |
|---|---|
| seconds | **approved**, **applying the fix**: the executor checks that the Deployment still matches what you approved, tries the rollback in a dry run, then applies it. |
| ~2–5 min | **checking the fix**, then **fixed**: every pod ready, no new out-of-memory kills, and stable for the whole check. |

The timeline holds the whole audit trail, from the alert to the verified outcome.

## Things to try

Run `make demo-fix` to heal checkout between runs, then `make demo-break` again.

- **Deny.** Enter a reason and click **Deny**. The incident closes without action, with your reason in the timeline.
- **Change the workload after the suggestion.** While it waits for approval, run `kubectl --context kind-themis-dev -n shop set env deploy/checkout EXTRA=x`. That doesn't fix anything, so within about a minute and a half Themis replaces the incident with a new one for the workload as it is now.
- **Fix it yourself.** While it awaits approval, run `make demo-fix`. Within about a minute and a half the incident resolves on its own: "Fixed outside Themis, by kubectl-rollout.", the name `kubectl rollout undo` writes with.
- **Break it twice.** Run `make demo-fix`, then `kubectl --context kind-themis-dev -n shop set env deploy/checkout MODE=leak` and, right after, `kubectl --context kind-themis-dev -n shop set env deploy/checkout EXTRA=x`. The fix Themis proposes rolls back to the first broken revision, so it doesn't help: after the check, Themis puts checkout back as it was before it acted, and hands the incident to a person.
- **Edit during verification.** Approve, then edit the Deployment while it is verifying. Themis says it can't tell whether the fix worked and hands the incident to a person: it never claims a success it can't prove.
- **GitOps.** `kubectl --context kind-themis-dev -n shop annotate deploy/checkout argocd.argoproj.io/tracking-id=x`, then break it. Policy denies any change, so the incident closes without action.
- **Escalation webhook.** Put a `url` key, and optionally a `token` key, in a Secret in namespace `themis`, then `helm upgrade` with `--reuse-values --set notifications.webhookSecret=<name>`. Every escalation is POSTed there as JSON.

## Clean up

```bash
make demo-down monitoring-down themis-down   # or simply: make kind-down
```

## Running locally

To work on Themis itself, run the three components on your machine against the kind cluster. Everything listens on `127.0.0.1` only.

```bash
make kind-up dev-db demo-up
export DATABASE_URL='postgres://themis:themis@127.0.0.1:5432/themis?sslmode=disable'
make run                                  # themis-core, in one terminal
make run-executor                         # themis-executor, in another
make run-intelligence                     # the investigator, against Ollama on 127.0.0.1:11434
make demo-break
make demo-alert                           # once a checkout pod is crash looping: posts the alert Alertmanager would
```

The UI is on <http://127.0.0.1:8080>, with the token in `bin/dev/admin-token`. Use `OLLAMA_URL=...` with `run-intelligence` to point it at another endpoint.
