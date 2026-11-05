# OOM limit too low

A Deployment's containers are killed for running out of memory on a version that has run for a while: more load or more data now needs more than their memory limit. Themis checks that the kills are real, on the current version, not brought by a recent rollout, and that the containers reached their limit, and may suggest raising the memory limit of the killed containers, which a person approves.

A metrics source helps but isn't needed. With Prometheus, Themis sees each container's peak memory and can tell a container that reached its limit from one killed for something else. Without one, the OOM kills in the pod states are enough to act on, and the size comes from the current limit.

## Alerts

`KubePodOOMKilled` and `KubePodCrashLooping`, about a Deployment. The rollout-regression pack answers them too, so an incident from them gets both packs: the investigator sees a rollback and a higher limit, with each pack's guidance, and each pack's policy decides its own fix. A rollback is right when a new version brought the kills; a higher limit when the version had run.

## Evidence

| Template | Why |
|---|---|
| `pod_states` | Which containers were OOMKilled, of which version, and how long they ran first |
| `container_memory` | Each container's memory now and at its peak over the last hour, against its limit, and which metrics source said so. Without a source, it is a gap that says why |
| `rollout_history` | Which version runs, since when, and each version's memory limits |
| `target_events` | Restarts, and evictions when the node runs short of memory |
| `referenced_config` | Whether the configuration the pods read changed |
| `container_logs_tail` | Asked for when the pods' own words help, such as memory growing while they run |

## Checks

| Check | True when |
|---|---|
| `oom_killed_current_revision` | A container of the current version was OOMKilled |
| `oom_without_recent_rollout` | The current version is OOMKilled and rolled out more than 6 hours ago. False within 6 hours, or without OOM kills |
| `memory_data_available` | The metrics source says each container's peak memory. False with only the Kubernetes metrics API, which keeps no history; unknown without a source |
| `memory_at_limit_before_kill` | A container peaked at 90% or more of its memory limit over the window. Unknown without peak figures |
| `oom_after_long_run` | Every OOM kill came after the container had run an hour or more |
| `evicted_for_node_memory` | A pod was evicted because its node was short of memory |

## Fix

`deployment.set-resources`, raising the memory limit of the killed containers, and their memory request with it if needed. The investigator decides by how much, from the evidence; its guidance suggests the peak plus 25% when there are figures, otherwise 1.5 times the current limit. Themis checks the values are valid and that the cluster can run them, never how big they are.

The policy enforces, whoever suggests the fix:

| Rule | Decision | Why |
|---|---|---|
| `raise-needs-oom-kills-on-a-version-that-has-run` | deny | Unless `oom_killed_current_revision` and `oom_without_recent_rollout` are true, `memory_at_limit_before_kill` isn't false, and `evicted_for_node_memory` isn't true: without OOM kills there is nothing to act on, right after a rollout the new version may be the cause, which is a rollback's question, a container that never reached its limit wasn't killed by it, and a node short of memory isn't helped by a higher limit |
| `raise-a-memory-limit-only` | deny | Unless the change raises a memory limit that exists, and does nothing else but raise a memory request: no CPU, no lower value, and no new limit where there was none |
| `raise-needs-approval` | require_approval | A person approves every raise, and its size |

There is no size cap: an operator who wants one adds a rule on `change["memory.ratio"]`. Putting back what Themis changed, after a raise that didn't help, lowers the limit again, and no rule here refuses it.

## Cases

| Case | What it is |
|---|---|
| `peak-at-limit-after-growth` | A ten-day-old version; a larger catalog loads at start, and containers are OOMKilled minutes later, at the 512Mi limit Prometheus shows them peak at. Raised by both packs' alert |
| `oom-kills-no-metrics` | Two workers OOMKilled minutes into a larger batch, with no metrics source: the pod states alone show the kills |

## Look-alikes

| Case | What it is |
|---|---|
| `oom-after-rollout` | A version rolled out 40 minutes ago lowered the limit, and is OOMKilled at it. Both packs answer: rolling back is right, and this pack's policy refuses the raise |
| `memory-grows-until-killed` | Containers climb while they run and are killed after five to six hours, again and again, with a limit that held for days, and logs showing the heap growing: a higher limit only delays the next kill. The checks can't tell this from steady growth, so the policy would let a raise reach a person; the guidance says to leave it to one |
| `node-memory-pressure` | The node ran short of memory: two pods evicted, and a container killed well under its limit |
| `killed-without-oom-evidence` | Containers killed with exit code 137, but no OOMKilled reason and no metrics source: raising the limit would be a guess |
