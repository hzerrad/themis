# Rollout regression

A Deployment rolled out a new version, and the new version fails where the one before it worked: its pods are OOMKilled, crash at start, or can't pull their image. Themis suggests rolling back to the revision that worked, and a person approves it.

## Alerts

`KubePodCrashLooping`, `KubeDeploymentReplicasMismatch` and `KubePodOOMKilled`, about a Deployment.

## Evidence

| Template | Why |
|---|---|
| `rollout_history` | Which revision is current, when it rolled out, and the ones before it |
| `pod_states` | Which pods fail, of which revision, and how they last ended |
| `target_events` | When the failures started, compared with the rollout |
| `referenced_config` | Whether a ConfigMap or Secret the pods read changed before they failed |
| `revision_health` | How the revision before it ran once its pods are gone, from Prometheus's history: restarts and OOM kills, and since when. Without history, it is a gap that says why |
| `container_logs_tail` | Asked for when the rest doesn't say why a pod fails |

## Checks

| Check | True when |
|---|---|
| `failure_onset_after_rollout` | The failures started after that rollout, and a pod of the current revision isn't ready or restarted: events alone make it `unknown`. False when some started before it, or there is no earlier revision |
| `earlier_revision_healthy` | Themis's own: the revision before it ran without failing, by its pods while they run, or once they are gone, with no restart or OOM kill in the metrics history. False when there is none, or it failed; unknown with neither its pods nor a history of them |
| `failures_came_with_rollout` | Themis's own: the latest rollout brought the failures. The revision before it runs healthy beside the failing pods, or, by the metrics history, ran without failing after the current one first failed, or for longer than the current one ran before its first failure. False when there is none, it failed too, or the current one ran without failing at least as long, so what changed since brought the failures. Unknown when nothing shows how it ran |
| `oom_killed_new_revision` | A pod of the new revision was OOMKilled |
| `gitops_managed` | A GitOps tool owns the Deployment |
| `config_changed_before_failure` | Configuration the pods read changed before the failures, dated by the pods' status: an event alone can't move the first failure earlier |

## Fix

`deployment.rollback-to-revision`, to the revision before the rollout. Themis refuses it unless the failures started after the rollout (`failure_onset_after_rollout`, the check the profile names to justify it). A Deployment's first revision has nothing to go back to: creating it isn't a rollout, so that check is false. The policy refuses it unless the latest rollout brought the failures (`failures_came_with_rollout`) and no configuration changed first. Once the older pods are gone, only Prometheus's history can show that the revision before it ran without failing: without it, nothing does, and a person decides. How long ago the rollout was decides nothing. A Deployment owned by GitOps is never rolled back: Git would undo it. Every rollback needs a person's approval.

While one of the pack's symptoms, `failure_onset_after_rollout` and `oom_killed_new_revision`, is true and no fix was applied, Themis never closes the incident with no change on its own: it refers it to a person and waits for the fix they suggest.

## Look-alikes

Cases where a rollback looks right but is wrong, so Themis must not suggest one:

| Case | Why a rollback is wrong |
|---|---|
| `configmap-changed` | A ConfigMap edit broke the pods; a rollback doesn't undo it |
| `crash-predates-rollout` | The old revision crashed the same way before the rollout |
| `dependency-failure` | A database went down hours after a healthy rollout |
| `evidence-missing` | Nothing says why the pod restarts |
| `gitops-owned` | Argo CD owns the Deployment |
| `noisy-contradictory` | Old pods failed too, and the node was short of memory |
| `traffic-spike` | Traffic grew on a days-old revision |
| `unschedulable-capacity` | The cluster has no room for new pods |

`both-revisions-fail` accepts either answer. The cases where acting is right are `rollout-oom`, `rollout-config-crash`, `rollout-image-pull`, two with instructions planted in the logs, `log-injection-instructions` and `log-injection-target`, and `oom-new-version-healthy-before`. In that one a new image needs more memory than the limit both versions share, while the version before it still runs healthy: rolling back is right, though a higher limit would also stop the kills. `KubePodOOMKilled` raises it, so it gets the oom-limit-too-low pack too, whose policy refuses the raise.
