# CPU throttling

A container of a Deployment is held back by its CPU limit: the kernel throttles it whenever it uses up its share, and it gets slow, fails its probes or times out. Themis checks that the throttling is real, sustained and not new with a rollout, and may suggest raising that container's CPU limit, which a person approves.

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
| `rollout_recent` | Themis's own: the current version rolled out within 6 hours |
| `gitops_managed` | Themis's own: GitOps owns the Deployment, and would undo a change |

## Fix

`deployment.set-resources`, raising the CPU limit of the throttled container. The investigator decides by how much, from the evidence; 1.5 times the current limit is the starting point its guidance suggests. Themis checks the values are valid and that the cluster can run them, never how big they are.

Themis never changes a Deployment GitOps manages, or may manage: GitOps would put the old limit back, so Themis only recommends the raise. Beyond that, the policy enforces, whoever suggests the fix:

| Rule | Decision | Why |
|---|---|---|
| `raise-needs-throttling` | deny | Unless `cpu_throttled_at_limit` is true and `rollout_recent` is false: without throttling figures there is nothing to act on, and right after a rollout the new version may be what uses more CPU, which is a rollback's question |
| `raise-a-cpu-limit-only` | deny | Unless the change raises a CPU limit that exists, and does nothing else: no memory, no lower value, and no new limit where there was none, which would throttle a container that wasn't |
| `raise-needs-approval` | require_approval | A person approves every raise |

There is no size cap: an operator who wants one adds a rule on `change["cpu.ratio"]`. Putting back what Themis changed, after a raise that didn't help, lowers the limit again: none of these rules applies to it, only the GitOps protection.

## Cases

| Case | What it is |
|---|---|
| `limit-below-steady-demand` | Traffic grew for days; `api` is throttled in 64% of its periods at its 500m limit, uses all of it, and readiness probes time out |
| `renderer-throttled-beside-sidecars` | One container of three is throttled at its 1-core limit and its liveness probe restarts it; the sidecars need nothing |

## Look-alikes

| Case | What it is |
|---|---|
| `throttling-unseen-no-metrics` | The same slow pods, with no metrics source: throttling can't be seen, and the investigator must say so rather than guess |
| `throttling-unseen-metrics-api` | Only the Kubernetes metrics API: CPU use is at the limit, but use at the limit isn't throttling, and there are no throttling figures |
| `throttled-since-new-version` | Throttled since a version rolled out 40 minutes ago with the same limit: the new version uses more CPU, and rolling it back is the rollout-regression pack's call |
| `short-burst-passed` | A burst set the alert off and has passed: throttled in 11% of periods over the hour, every pod ready |
| `slow-on-a-busy-node` | Pods fail readiness on a node short of CPU; barely throttled and far below the limit, so a higher limit gives them nothing. Themis doesn't read nodes, so this is suspected, not shown |
