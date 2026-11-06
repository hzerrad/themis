# CPU throttling

A container of a Deployment is held back by its CPU limit: the kernel throttles it whenever it uses up its share, and it gets slow, fails its probes or times out. Themis checks that the throttling is real, sustained and not brought by a new version, and may suggest raising that container's CPU limit, which a person approves.

Whether a new version brought it depends on whether there is a healthy earlier version to go back to. When the version before it still runs healthy, the new one may use more CPU, and the call is a person's. When there is no earlier version, as for a Deployment created minutes ago, or the one before it failed too, a higher limit is the fix, however recently the version rolled out. When the earlier version's pods are gone and its health can't be seen, a raise waits until 6 hours after the rollout, as `rollout_recent` says.

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
| `container_logs_tail` | Asked for when the pods' own words help, such as timeouts |

## Checks

| Check | True when |
|---|---|
| `throttling_data_available` | The metrics source says how often the containers were throttled. False with only the Kubernetes metrics API, which keeps no history |
| `cpu_throttled_at_limit` | A container with a CPU limit was throttled in 25% or more of its CPU periods over the window, the alert's own threshold. Unknown without throttling figures |
| `cpu_usage_near_limit` | A container with a CPU limit uses 80% or more of it now |
| `rollout_recent` | Themis's own: the current version replaced an earlier one within 6 hours. False for a Deployment's first revision |
| `earlier_revision_healthy` | Themis's own: the revision before the current one runs ready pods that never failed. False when there is none, or its pods failed; unknown once its pods are gone |
| `gitops_managed` | Themis's own: GitOps owns the Deployment, and would undo a change |

## Fix

`deployment.set-resources`, raising the CPU limit of the throttled container. The investigator decides by how much, from the evidence; 1.5 times the current limit is the starting point its guidance suggests. Themis checks the values are valid and that the cluster can run them, never how big they are.

Themis never changes a Deployment GitOps manages, or may manage: GitOps would put the old limit back, so Themis only recommends the raise. It refuses a raise unless `cpu_throttled_at_limit`, the check the profile names to justify it, is true: without throttling figures there is nothing to act on. Beyond that, the policy enforces, whoever suggests the fix:

| Rule | Decision | Why |
|---|---|---|
| `no-raise-for-a-new-version` | deny | When `earlier_revision_healthy` is true, or unknown and `rollout_recent` isn't false. A healthy version before it means the new one may be what uses more CPU, which is a rollback's question, and so may one whose health can't be seen right after the rollout. With no earlier version, or one that failed too, a raise is the fix, however recent the version |
| `raise-a-cpu-limit-only` | deny | Unless the change raises a CPU limit that exists, and does nothing else: no memory, no lower value, and no new limit where there was none, which would throttle a container that wasn't |
| `raise-needs-approval` | require_approval | A person approves every raise |

There is no size cap: an operator who wants one adds a rule on `change["cpu.ratio"]`. Putting back what Themis changed, after a raise that didn't help, lowers the limit again: none of these rules applies to it, only the GitOps protection.

While `cpu_throttled_at_limit`, the pack's symptom, is true and no fix was applied, Themis never closes the incident with no change on its own: it refers it to a person, saying which fix the checks support or what they found, and waits for the fix they suggest.

## Cases

| Case | What it is |
|---|---|
| `limit-below-steady-demand` | Traffic grew for days; `api` is throttled in 64% of its periods at its 500m limit, uses all of it, and readiness probes time out |
| `renderer-throttled-beside-sidecars` | One container of three is throttled at its 1-core limit and its liveness probe restarts it; the sidecars need nothing |
| `throttled-first-revision` | A Deployment created 29 minutes ago, with only revision 1, is sent more work than its 200m limit allows: throttled in 90% of its periods, and its logs say how far behind it falls. No new version brought it, so raising the limit is right |

## Look-alikes

| Case | What it is |
|---|---|
| `throttling-unseen-no-metrics` | The same slow pods, with no metrics source: throttling can't be seen, and the investigator must say so rather than guess |
| `throttling-unseen-metrics-api` | Only the Kubernetes metrics API: CPU use is at the limit, but use at the limit isn't throttling, and there are no throttling figures |
| `throttled-since-new-version` | Throttled since a version rolled out 40 minutes ago with the same limit. The version before it ran nine days, and its pods are gone, so its health can't be seen: the new version may use more CPU, and rolling it back is the rollout-regression pack's call |
| `short-burst-passed` | A burst set the alert off and has passed: throttled in 11% of periods over the hour, every pod ready |
| `slow-on-a-busy-node` | Pods fail readiness on a node short of CPU; barely throttled and far below the limit, so a higher limit gives them nothing. Themis doesn't read nodes, so this is suspected, not shown |
