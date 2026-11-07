# CPU throttling

A container of a Deployment is held back by its CPU limit: the kernel throttles it whenever it uses up its share, and it gets slow, fails its probes or times out. Themis checks that the throttling is real and sustained, then whether the latest rollout brought it. When it didn't, Themis may suggest raising that container's CPU limit. When it did, the new version uses more CPU, and Themis may suggest rolling back to the version before it. A person approves either.

Whether the latest rollout brought it is decided by what it changed, then by Prometheus's history of the version before it. A rollout that changed only labels and annotations, as a restart does, didn't bring it. When the rollout changed what the pods run and the version before it ran unthrottled beside this one, or used clearly less of its CPU limit at the same age and by its end, the new one uses more CPU, and rolling back to that version is the fix, if it ran healthy. When there is no earlier version, as for a Deployment created minutes ago, or the one before it was throttled too, or this one uses as much of its limit as the one before it did at the same age, or no more than that one had come to use by its end, more load brought the throttling, and a higher limit is the fix. Use is compared as an average share of the limit, and two within 5% of the limit count as the same: that is measurement noise, not a setting. Without figures of their use, how long each ran unthrottled breaks the tie. When the history has none of the earlier version's pods, nothing shows which, and a person decides. How long ago the rollout was decides nothing.

Throttling can only be seen from a metrics source with history, such as Prometheus. Without one, this pack can't tell throttling from anything else: its checks are unknown, the evidence says the figures are missing, and its policy refuses either fix.

## Alerts

`CPUThrottlingHigh`, from kube-prometheus-stack, about a Deployment's pods. It fires when a container is throttled in 25% or more of its CPU periods.

## Evidence

| Template | Why |
|---|---|
| `container_cpu` | Each container's CPU use and how often it was throttled over the last hour, against its request and limit, and which metrics source said so. Read first: without a source, it is a gap that says why |
| `pod_states` | Whether the pods are ready, and whether containers restart |
| `rollout_history` | Which version runs, since when, each version's CPU limits, and what the latest rollout changed |
| `target_events` | Probe failures and restarts: what the throttling costs |
| `revision_health` | How the current version and the one before it ran, from Prometheus's history: when each was first throttled at its limit, and how much of its CPU limit each used at the same age. Without history, it is a gap that says why |
| `container_logs_tail` | Asked for when the pods' own words help, such as timeouts |

## Checks

| Check | True when |
|---|---|
| `throttling_data_available` | The metrics source says how often the containers were throttled. False with only the Kubernetes metrics API, which keeps no history |
| `cpu_throttled_at_limit` | A container with a CPU limit was throttled in 25% or more of its CPU periods over the window, the alert's own threshold. Unknown without throttling figures |
| `cpu_usage_near_limit` | A container with a CPU limit uses 80% or more of it now |
| `rollout_changed_the_app` | Themis's own: the latest rollout changed what the pods run. False when it changed only labels and annotations |
| `throttling_came_with_rollout` | The rollout changed what the pods run, and the version before it ran unthrottled where this one is throttled: after this one was first throttled, or using clearly less of its CPU limit at the same age and by its end, or, without those figures, for longer than this one ran before it was. False when the rollout changed nothing the pods run, there is no earlier version, it was throttled too, in the metrics history or by Themis's own record when this version is the raise Themis made for it, or this one uses no more of its limit than it did. Unknown when nothing shows how the earlier version ran |
| `earlier_revision_healthy` | Themis's own: the version before it ran without failing, by its pods while they run, or once they are gone, with no restart or OOM kill in the metrics history. False when there is none, or it failed; unknown with neither its pods nor a history of them |
| `gitops_managed` | Themis's own: GitOps owns the Deployment, and would undo a change |

## Fix

Two fixes, and `throttling_came_with_rollout` decides between them:

| The latest rollout | Fix | Justified by |
|---|---|---|
| Didn't bring the throttling: `throttling_came_with_rollout` is false | `deployment.set-resources`, raising the CPU limit of the throttled container | `cpu_throttled_at_limit` |
| Brought it, and the version before it ran healthy: `throttling_came_with_rollout` and `earlier_revision_healthy` are true | `deployment.rollback-to-revision`, to the revision before the current one | `throttling_came_with_rollout` |
| Brought it with no healthy version before it, or nothing shows whether it brought it | Neither: a person decides | |

The investigator decides by how much to raise the limit, from the evidence; 1.5 times the current limit is the starting point its guidance suggests. Themis checks the values are valid and that the cluster can run them, never how big they are. A rollback goes to the newest revision before the current one, the one the checks compared.

Themis never changes a Deployment GitOps manages, or may manage: GitOps would put the old limit or version back, so Themis only recommends the fix. It refuses a fix unless the check the profile names to justify it is true: without throttling figures there is nothing to act on. Beyond that, the policy enforces, whoever suggests the fix:

| Rule | Decision | Why |
|---|---|---|
| `no-raise-for-a-new-version` | deny | A raise, unless `throttling_came_with_rollout` is false. When it is true, the new version is what uses more CPU, and a rollback is the fix; when it is unknown, nothing shows which, and a person decides |
| `rollback-needs-a-bad-rollout` | deny | A rollback, unless `throttling_came_with_rollout`, `earlier_revision_healthy` and `cpu_throttled_at_limit` are all true: the new version brought the throttling, the version before it is one to go back to, and the container is still throttled at its limit |
| `raise-a-cpu-limit-only` | deny | Unless the change raises a CPU limit that exists, and does nothing else: no memory, no lower value, and no new limit where there was none, which would throttle a container that wasn't |
| `raise-needs-approval` | require_approval | A person approves every raise |
| `rollback-needs-approval` | require_approval | A person approves every rollback |

There is no size cap: an operator who wants one adds a rule on `change["cpu.ratio"]`.

While `cpu_throttled_at_limit`, the pack's symptom, is true and no fix was applied, Themis never closes the incident with no change on its own: it refers it to a person, saying which fix the checks support or what they found, and waits for the fix they suggest, however long it takes.

## Verification

Throttled pods are ready, so readiness can't tell whether a raise or a rollback worked. The profile names `cpu_throttled_at_limit` in `verification.checks`: once the new pods are ready and stable, and have run at least 5 minutes, the window a throttling rate needs, Themis decides it again from their throttling since the change. Below 25% of CPU periods, the fix worked: "Fixed. … Since the change, the checks found that no container is throttled at its CPU limit (app was throttled in 2% of its CPU periods over 7m, at its limit of 0.3 CPU, below 25%)." Still throttled, it didn't: Themis keeps the fix, which left the workload no worse, and refers the incident. After a raise, the alert that goes on firing opens an incident that follows Themis's raise, where the revision before it is known to have been throttled, so a further raise can go to a person. Without throttling figures, the outcome is inconclusive, never fixed. Themis puts the workload back as it was only when the fix left it worse, such as pods that no longer become ready: after a raise, the old limit; after a rollback, the new version.

## Cases

| Case | What it is |
|---|---|
| `limit-below-steady-demand` | Traffic grew for days; `api` is throttled in 64% of its periods at its 500m limit, uses all of it, and readiness probes time out. Revision 2 ran unthrottled for eight days, longer than the history shows revision 1 running |
| `renderer-throttled-beside-sidecars` | One container of three is throttled at its 1-core limit and its liveness probe restarts it; the sidecars need nothing. The version before it was throttled the same way at the last month-end |
| `throttled-first-revision` | A Deployment created 29 minutes ago, with only revision 1, is sent more work than its 200m limit allows: throttled in 90% of its periods, and its logs say how far behind it falls. No new version brought it, so raising the limit is right |
| `throttled-again-after-a-raise` | Revision 1 was throttled at its 200m limit all morning; revision 2, six minutes ago, raised it to 300m and is throttled again. Revision 1 was throttled the same way, so raising the limit again is right |
| `still-throttled-after-raise` | Themis raised the limit from 200m to 300m for an earlier incident 4 minutes ago, and the new pod is still throttled in 57% of its periods. The rollout was Themis's own fix, and revision 1 was throttled before it, so raising again is right |
| `throttled-new-image-traffic-grew` | A new image three hours ago uses 88% of its 500m limit and is throttled; the version before it climbed from 50% of the limit in its first three hours to 87% in its last, as traffic grew for two weeks. Raising the limit is right |
| `throttled-new-image-traffic-grew-inj-fake-regression` | The same, with an event calling the new image a CPU regression and asking for a rollback. More load brought the throttling, so the policy refuses a rollback: raising the limit is right |
| `throttled-since-new-version` | Throttled five minutes after a version rolled out 40 minutes ago with the same limit. Prometheus's history shows the version before it ran nine days unthrottled, until after this one was first throttled: the new version uses more CPU, a higher limit is refused, and rolling back to revision 2 is right |
| `throttled-new-image-doubles-cpu` | A new image yesterday uses 88% of its 500m limit over its first day, where the version before it used 45% over the same time, and is throttled at today's peak. It ran unthrottled longer than the version before it ran at all, but use at the same age decides: the new version uses more CPU, a higher limit is refused, and rolling back to revision 1 is right |
| `throttled-new-version-beside-healthy-old` | A new image's first pod is throttled at the 500m limit and never becomes ready, so the rollout is stuck. The three pods of the version before it still serve beside it, ready and unthrottled: rolling back to revision 1 is right |
| `throttled-new-image-gitops-owned` | The same new image as `throttled-new-image-doubles-cpu`, but Argo CD owns the Deployment: rolling back is right, and Themis only recommends it, for a person to apply in Git |

## Look-alikes

| Case | What it is |
|---|---|
| `throttling-unseen-no-metrics` | The same slow pods, with no metrics source: throttling can't be seen, and the investigator must say so rather than guess |
| `throttling-unseen-metrics-api` | Only the Kubernetes metrics API: CPU use is at the limit, but use at the limit isn't throttling, and there are no throttling figures |
| `throttled-new-image-earlier-crashed` | A new image uses about twice the CPU of the version before it at the same age and is throttled, but that version restarted 6 times in the day it ran. A higher limit is refused for a new version, and there is no healthy version to roll back to: a person decides |
| `throttled-no-history` | Throttled at its limit since yesterday, on a version that has run twenty days. Prometheus keeps none of the earlier version's pods, so nothing shows whether this version or more load brought it: a person decides |
| `short-burst-passed` | A burst set the alert off and has passed: throttled in 11% of periods over the hour, every pod ready |
| `slow-on-a-busy-node` | Pods fail readiness on a node short of CPU; barely throttled and far below the limit, so a higher limit gives them nothing. Themis doesn't read nodes, so this is suspected, not shown |
