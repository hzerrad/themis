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
# or, for Claude: MODEL_PROVIDER=anthropic MODEL_NAME=claude-haiku-5-5 MODEL_KEY_SECRET=model-key
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
make monitoring-up                            # Prometheus, kube-state-metrics and Alertmanager, first, so the chart finds Alertmanager
make themis-up MODEL_URL=... MODEL_NAME=...   # builds the images, loads them into kind, installs the chart with the executor
make demo-up                                  # the healthy checkout, catalog and search workloads in namespace shop
```

`monitoring-up` installs the alert rules every scenario pack answers, the ones Themis ships, but faster, as [`scripts/dev/monitoring/fast.yaml`](../scripts/dev/monitoring/fast.yaml) says: `KubePodCrashLooping` fires after 30 seconds instead of the usual 15 minutes, so the demo moves at a watchable pace. `themis-up` points Themis at its Prometheus, for the containers' memory and CPU figures.

## 4. Open the UI

```bash
make ui   # serves the UI on http://localhost:8090
```

Open it and sign in with the initial admin password. `make ui` prints the command that reads it, and reconnects by itself when the core restarts, such as after a `helm upgrade`. Themis starts once an admin account exists. Create yours with a password of your own. Creating it signs you in. Themis reads no alerts before that. The list is empty for now.

## 5. Break the rollout

```bash
make demo-break
```

What happens next, and roughly when:

| After | You see |
|---|---|
| ~1 min | A checkout pod crash-loops. Themis reads the alert from Alertmanager and an incident appears, **investigating**. |
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

- **Deny, then suggest another fix.** Enter a reason and click **Deny the rollback**. The incident stays open, waiting for another fix, with your reason in the timeline, as you wrote it. Suggest another fix from the form below it, then approve it yourself: the timeline records that you suggested and approved it. **Deny and close** closes the incident with no change instead.
- **Deny, then investigate again.** Deny with a reason, such as "The new revision carries a schema migration: don't roll back.", then click **Investigate again**. The investigator reads which fix you denied, and your reason, as a note from a person: text to weigh against the evidence, never instructions. Whatever it suggests next passes the same checks, policy and approval. The new investigation uses what is left of the incident's token budget. Once that is spent, the button says so.
- **Suggest a fix yourself.** While the incident is **investigating**, open **Suggest a fix while Themis investigates**. A crash-looping checkout matches three scenario packs, so the form offers a rollback, a memory and CPU change and a probe change. Each starts from checkout as it is now: its earlier revisions, its limits and requests, its probe timings. Themis uses your suggestion if the model suggests none, and checks it exactly as it checks the model's. One the checks don't justify is refused, with the reason, and the incident waits for another fix.
- **Change the workload after the suggestion.** While it waits for approval, run `kubectl --context kind-themis-dev -n shop set env deploy/checkout EXTRA=x`. That doesn't fix anything, so within about a minute and a half Themis replaces the incident with a new one for the workload as it is now.
- **Fix it yourself.** While it awaits approval, run `make demo-fix`. Within about a minute and a half the incident resolves on its own: "Fixed outside Themis, by kubectl-rollout.", the name `kubectl rollout undo` writes with.
- **Break it twice.** Run `make demo-fix`, then `kubectl --context kind-themis-dev -n shop set env deploy/checkout MODE=leak` and, right after, `kubectl --context kind-themis-dev -n shop set env deploy/checkout EXTRA=x`. The fix Themis proposes rolls back to the first broken revision, so it doesn't help: after the check, Themis puts checkout back as it was before it acted, and hands the incident to a person.
- **Edit during verification.** Approve, then edit the Deployment while it is verifying. Themis says it can't tell whether the fix worked and hands the incident to a person: it never claims a success it can't prove.
- **GitOps.** `kubectl --context kind-themis-dev -n shop annotate deploy/checkout argocd.argoproj.io/tracking-id=x`, then break it. Policy denies any change, so the incident closes without action.
- **Escalation webhook.** Put a `url` key, and optionally a `token` key, in a Secret in namespace `themis`, then `helm upgrade` with `--reuse-values --set notifications.webhookSecret=<name>`. Every escalation is POSTed there as JSON.

### Every scenario pack

Each [scenario pack](scenarios.md) has a break of its own in `shop`, and a target that undoes it. The incident page names the packs that answered under **Scenario packs**. An alert several packs answer brings them all, and the investigator chooses among their fixes.

- **rollout-regression.** `make demo-break`, the bad rollout above. Themis proposes rolling checkout back to the revision before. `make demo-fix` undoes it.
- **oom-limit-too-low.** `make demo-break-oom` doubles the data catalog loads at start, to 64Mi, and restarts its pods, with no rollout. They peak at 95% of their 60Mi limit and are OOMKilled 3.5 minutes after each start. `KubePodOOMKilled` opens the incident about 5 minutes after the command, and Themis proposes a higher memory limit, for your approval. Under **What Themis checked**: the OOM kills, the peak Prometheus measured, and that catalog has no earlier revision to roll back to. `make demo-fix-oom` undoes it.
- **cpu-throttling.** `make demo-break-cpu` sends search 250m of CPU work, above its 200m limit, with no rollout. The kernel throttles it in nearly every period, and its logs say how far behind it falls. `CPUThrottlingHigh` opens the incident about 5.5 minutes after the command, once the throttling covers most of the 5 minutes over which Themis reads CPU use. Themis proposes a higher CPU limit, for your approval. Under **What Themis checked**: the share of periods throttled, the CPU use against the limit, and that search has no earlier revision to go back to. `make demo-fix-cpu` undoes it.
- **probe-restart-storm.** `make demo-break-probes` deploys inventory, whose health check answers in 2 seconds and whose liveness probe allows 1. The kubelet restarts containers that work. `KubePodCrashLooping` opens the incident about 3 minutes later, and Themis proposes a longer liveness timeout, for your approval. Under **Evidence**: probe failures that are all timeouts, and logs where every health check answers. `make demo-fix-probes` sets the timeout to 3 seconds.
- **stuck-pods.** `make demo-break-stuck` rolls checkout out to `busybox:1.73`, a tag that doesn't exist. The old pods keep serving. `KubeContainerWaiting` opens the incident about a minute and a half later, and `KubeDeploymentRolloutStuck` joins it after checkout's 10-minute progress deadline. Themis finds that the image can't be pulled and refers the incident to a person: it has no fix for stuck pods. `KubePodNotReady` opens a second incident for checkout, under probe-restart-storm, and its checks find the same: Themis refers it to a person too, and waits for the fix they suggest. `make demo-fix` rolls checkout back.

Both catalog and search break with no rollout, as load does, and run their first revision: no new version can have brought the failure, so a higher limit is the fix. A new version may be what needs more memory or CPU, so the policy refuses a higher limit while the version before it runs healthy, and, when that version's pods are gone and its health can't be seen, within 6 hours of the rollout. `make demo-break-oom` and `make demo-break-cpu` apply the manifest as shipped, which after Themis raised a limit is a rollout over the raised version. Themis then refuses a higher limit and refers the incident to a person; for catalog, the checks support rolling back to the raised version instead. To see the raise again, delete the Deployment, `kubectl --context kind-themis-dev -n shop delete deploy catalog` or `search`, and break it: it comes back at its first revision.

## Clean up

```bash
make demo-down monitoring-down themis-down   # or simply: make kind-down
```

## Running locally

To work on Themis itself, run the three components on your machine against the kind cluster. Everything listens on `127.0.0.1` only.

```bash
make kind-up dev-db demo-up
make run                                  # themis-core, in one terminal
make run-executor                         # themis-executor, in another
make run-intelligence                     # the investigator, against Ollama on 127.0.0.1:11434
make demo-break
make demo-alert                           # once a checkout pod is crash looping: posts the alert Alertmanager would
```

`make run` and `make run-executor` use the `make dev-db` database; set `DATABASE_URL` to use another.

| Process | Port | Serves |
|---|---|---|
| themis-core | 8080 | UI, API and alert intake |
| | 9090 | gRPC for the investigator |
| | 8081 | `/healthz`, `/readyz` |
| | 8082 | `/metrics` |
| themis-executor | 9091 | gRPC for themis-core |
| | 8091 | `/healthz`, `/readyz` |
| | 8092 | `/metrics` |
| investigator | none | its `/metrics` is off locally |
| PostgreSQL (`make dev-db`) | 5432 | |

The UI is on <http://127.0.0.1:8080>. Sign in as `dev` with the password in `bin/dev/admin-password`. `make run` creates that admin account at startup. The break-glass token is in `bin/dev/admin-token`.

`make run-intelligence` reads the same `MODEL_*` settings as the investigator in the cluster. Without them it uses Ollama: `OLLAMA_URL=...` points it at another Ollama, `MODEL_NAME=...` picks the model. For a hosted model, put the API key in a file:

```bash
make run-intelligence MODEL_PROVIDER=anthropic MODEL_NAME=claude-haiku-5-5 MODEL_API_KEY_FILE=$HOME/.config/themis/anthropic.key
make run-intelligence MODEL_BASE_URL=https://api.openai.com/v1 MODEL_NAME=<model> MODEL_API_KEY_FILE=$HOME/.config/themis/openai.key \
  MODEL_OUTPUT_LIMIT_FIELD=max_completion_tokens MODEL_TEMPERATURE=default MODEL_SCHEMA_DIALECT=openai-strict
```
