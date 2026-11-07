# CPU throttling

A container of a Deployment is held back by its CPU limit: the kernel throttles it whenever it uses up its share, and it gets slow, fails its probes or times out. Themis checks that the throttling is real, sustained and not brought by a new version, and may suggest raising that container's CPU limit, which a person approves.

Whether the latest rollout brought it is read from Prometheus's history of the version before it. When that version ran unthrottled where this one is throttled, the new one may use more CPU, and the call is a person's. When there is no earlier version, as for a Deployment created minutes ago, or the one before it was throttled too, or this one ran unthrottled at least as long as the one before it did, so more load since brought the throttling, a higher limit is the fix. When the history has none of the earlier version's pods, nothing shows which, and a person decides. How long ago the rollout was decides nothing.

Throttling can only be seen from a metrics source with history, such as Prometheus. Without one, this pack can't tell throttling from anything else: its checks are unknown, the evidence says the figures are missing, and its policy refuses any raise.

## Alerts

`CPUThrottlingHigh`, from kube-prometheus-stack, about a Deployment's pods. It fires when a container is throttled in 25% or more of its CPU periods.

## Evidence

| Template | Why |
|---|---|
| `container_cpu` | Each container's CPU use and how often it was throttled over the last hour, against its request and limit, and which metrics source said so. Read first: without a source, it is a gap that says why |
| `pod_states` | Whether the pods are ready, and whether containers restart |
| `rollout_history` | Which version runs, since when, and each version's CPU limits |
| `target_events` | Probe failures and restarts: what the throttling costs |
| `revision_health` | How the current version and the one before it ran, from Prometheus's history: when each was first throttled at its limit. Without history, it is a gap that says why |
| `container_logs_tail` | Asked for when the pods' own words help, such as timeouts |

## Checks

| Check | True when |
|---|---|
| `throttling_data_available` | The metrics source says how often the containers were throttled. False with only the Kubernetes metrics API, which keeps no history |
| `cpu_throttled_at_limit` | A container with a CPU limit was throttled in 25% or more of its CPU periods over the window, the alert's own threshold. Unknown without throttling figures |
| `cpu_usage_near_limit` | A container with a CPU limit uses 80% or more of it now |
| `throttling_came_with_rollout` | The version before it ran unthrottled where this one is throttled: after this one was first throttled, or for longer than this one ran before it was. False when there is no earlier version, it was throttled too, in the metrics history or by Themis's own record when this version is the raise Themis made for it, or this one ran unthrottled at least as long as it did. Unknown when nothing shows how the earlier version ran |
| `gitops_managed` | Themis's own: GitOps owns the Deployment, and would undo a change |

## Fix

`deployment.set-resources`, raising the CPU limit of the throttled container. The investigator decides by how much, from the evidence; 1.5 times the current limit is the starting point its guidance suggests. Themis checks the values are valid and that the cluster can run them, never how big they are.

Themis never changes a Deployment GitOps manages, or may manage: GitOps would put the old limit back, so Themis only recommends the raise. It refuses a raise unless `cpu_throttled_at_limit`, the check the profile names to justify it, is true: without throttling figures there is nothing to act on. Beyond that, the policy enforces, whoever suggests the fix:

| Rule | Decision | Why |
|---|---|---|
| `no-raise-for-a-new-version` | deny | Unless `throttling_came_with_rollout` is false. When it is true, the new version may be what uses more CPU, which is a rollback's question; when it is unknown, nothing shows which, and a person decides |
| `raise-a-cpu-limit-only` | deny | Unless the change raises a CPU limit that exists, and does nothing else: no memory, no lower value, and no new limit where there was none, which would throttle a container that wasn't |
| `raise-needs-approval` | require_approval | A person approves every raise |

There is no size cap: an operator who wants one adds a rule on `change["cpu.ratio"]`.

While `cpu_throttled_at_limit`, the pack's symptom, is true and no fix was applied, Themis never closes the incident with no change on its own: it refers it to a person, saying which fix the checks support or what they found, and waits for the fix they suggest, however long it takes.

## Verification

Throttled pods are ready, so readiness can't tell whether a raise worked. The profile names `cpu_throttled_at_limit` in `verification.checks`: once the new pods are ready and stable, and have run at least 5 minutes, the window a throttling rate needs, Themis decides it again from their throttling since the change. Below 25% of CPU periods, the raise fixed it: "Fixed. … Since the change, the checks found that no container is throttled at its CPU limit (app was throttled in 2% of its CPU periods over 7m, at its limit of 0.3 CPU, below 25%)." Still throttled, it didn't: Themis keeps the raise, which left the workload no worse, and refers the incident. The alert that goes on firing opens an incident that follows Themis's raise, where the revision before it is known to have been throttled, so a further raise can go to a person. Without throttling figures, the outcome is inconclusive, never fixed. Putting the old limit back happens only when the raise left the workload worse, such as pods that no longer become ready.

## Cases

| Case | What it is |
|---|---|
| `limit-below-steady-demand` | Traffic grew for days; `api` is throttled in 64% of its periods at its 500m limit, uses all of it, and readiness probes time out. Revision 2 ran unthrottled for eight days, longer than the history shows revision 1 running |
| `renderer-throttled-beside-sidecars` | One container of three is throttled at its 1-core limit and its liveness probe restarts it; the sidecars need nothing. The version before it was throttled the same way at the last month-end |
| `throttled-first-revision` | A Deployment created 29 minutes ago, with only revision 1, is sent more work than its 200m limit allows: throttled in 90% of its periods, and its logs say how far behind it falls. No new version brought it, so raising the limit is right |
| `throttled-again-after-a-raise` | Revision 1 was throttled at its 200m limit all morning; revision 2, six minutes ago, raised it to 300m and is throttled again. Revision 1 was throttled the same way, so raising the limit again is right |
| `still-throttled-after-raise` | Themis raised the limit from 200m to 300m for an earlier incident 4 minutes ago, and the new pod is still throttled in 57% of its periods. The rollout was Themis's own fix, and revision 1 was throttled before it, so raising again is right |

## Look-alikes

| Case | What it is |
|---|---|
| `throttling-unseen-no-metrics` | The same slow pods, with no metrics source: throttling can't be seen, and the investigator must say so rather than guess |
| `throttling-unseen-metrics-api` | Only the Kubernetes metrics API: CPU use is at the limit, but use at the limit isn't throttling, and there are no throttling figures |
| `throttled-since-new-version` | Throttled five minutes after a version rolled out 40 minutes ago with the same limit. Prometheus's history shows the version before it ran nine days unthrottled: the new version uses more CPU, and rolling it back is the rollout-regression pack's call |
| `throttled-no-history` | Throttled at its limit since yesterday, on a version that has run twenty days. Prometheus keeps none of the earlier version's pods, so nothing shows whether this version or more load brought it: a person decides |
| `short-burst-passed` | A burst set the alert off and has passed: throttled in 11% of periods over the hour, every pod ready |
| `slow-on-a-busy-node` | Pods fail readiness on a node short of CPU; barely throttled and far below the limit, so a higher limit gives them nothing. Themis doesn't read nodes, so this is suspected, not shown |
