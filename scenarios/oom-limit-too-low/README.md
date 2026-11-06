# OOM limit too low

A Deployment's containers are killed for running out of memory: more load or more data now needs more than their memory limit. Themis checks that the kills are real, on the current version, that no new version brought them, and that the containers reached their limit, and may suggest raising the memory limit of the killed containers, which a person approves.

What decides whether a new version brought the kills is whether there is a healthy earlier version to go back to. When the version before it still runs healthy, the new one brought them, and rolling back is the rollout-regression pack's fix. When there is no earlier version, as for a Deployment created minutes ago, or the one before it failed too, a higher limit is the fix, however recently the version rolled out. When the earlier version's pods are gone and its health can't be seen, a raise waits until 6 hours after the rollout, as `rollout_recent` says.

A metrics source helps but isn't needed. With Prometheus, Themis sees each container's peak memory and can tell a container that reached its limit from one killed for something else. Without one, the OOM kills in the pod states are enough to act on, and the size comes from the current limit.

## Alerts

`KubePodOOMKilled` and `KubePodCrashLooping`, about a Deployment. The rollout-regression pack answers them too, so an incident from them gets both packs: the investigator sees a rollback and a higher limit, with each pack's guidance, and each pack's policy decides its own fix. A rollback is right when a new version brought the kills and the one before it is healthy; a higher limit otherwise.

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
| `earlier_revision_healthy` | Themis's own: the revision before the current one runs ready pods that never failed. False when there is none, or its pods failed; unknown once its pods are gone |
| `rollout_recent` | Themis's own: the current revision replaced an earlier one within 6 hours. False for a Deployment's first revision |
| `memory_data_available` | The metrics source says each container's peak memory. False with only the Kubernetes metrics API, which keeps no history; unknown without a source |
| `memory_at_limit_before_kill` | A container peaked at 90% or more of its memory limit over the window. Unknown without peak figures |
| `oom_after_long_run` | Every OOM kill came after the container had run an hour or more |
| `evicted_for_node_memory` | A pod was evicted because its node was short of memory: an event says so, and the pod has failed. An event alone makes it `unknown` |

## Fix

`deployment.set-resources`, raising the memory limit of the killed containers, and their memory request with it if needed. The investigator decides by how much, from the evidence; its guidance suggests the peak plus 25% when there are figures, otherwise 1.5 times the current limit. Themis checks the values are valid and that the cluster can run them, never how big they are.

Themis refuses a raise unless `oom_killed_current_revision`, the check the profile names to justify it, is true: without OOM kills there is nothing to act on. Beyond that, the policy enforces, whoever suggests the fix:

| Rule | Decision | Why |
|---|---|---|
| `raise-needs-oom-kills-no-new-version-brought` | deny | When `earlier_revision_healthy` is true, or unknown and `rollout_recent` isn't false; when `memory_at_limit_before_kill` is false; or unless `evicted_for_node_memory` is false. A healthy version before it means the new one brought the kills, which is a rollback's question, and so may one whose health can't be seen right after the rollout. A container that never reached its limit wasn't killed by it, and a node short of memory isn't helped by a higher limit |
| `raise-a-memory-limit-only` | deny | Unless the change raises a memory limit that exists, and does nothing else but raise a memory request: no CPU, no lower value, and no new limit where there was none |
| `raise-needs-approval` | require_approval | A person approves every raise, and its size |

There is no size cap: an operator who wants one adds a rule on `change["memory.ratio"]`. Putting back what Themis changed, after a raise that didn't help, lowers the limit again, and no rule here refuses it.

While `oom_killed_current_revision` or `evicted_for_node_memory`, the pack's symptoms, is true and no fix was applied, Themis never closes the incident with no change on its own: it refers it to a person, saying which fix the checks support or what they found, and waits for the fix they suggest.

## Cases

| Case | What it is |
|---|---|
| `peak-at-limit-after-growth` | A ten-day-old version; a larger catalog loads at start, and containers are OOMKilled minutes later, at the 512Mi limit Prometheus shows them peak at. Raised by both packs' alert |
| `oom-kills-no-metrics` | Two workers OOMKilled minutes into a larger batch, with no metrics source: the pod states alone show the kills |
| `oom-first-revision` | A Deployment created 25 minutes ago, with only revision 1, is OOMKilled at 95% of its 60Mi limit. There is nothing to roll back to, so raising the limit is right. Raised by both packs' alert |
| `oom-both-versions` | A version rolled out 30 minutes ago is OOMKilled at the limit it kept, and so are the pods of the version before it, since before the rollout. A rollback goes back to a version that fails the same way: raising the limit is right |

## Look-alikes

| Case | What it is |
|---|---|
| `oom-after-rollout` | A version rolled out 40 minutes ago lowered the limit, and is OOMKilled at it, while the version before it still runs healthy. Both packs answer: rolling back is right, and this pack's policy refuses the raise. The rollout-regression pack's `oom-new-version-healthy-before` is the same with the limit unchanged |
| `memory-grows-until-killed` | Containers climb while they run and are killed after five to six hours, again and again, with a limit that held for days, and logs showing the heap growing: a higher limit only delays the next kill. The checks can't tell this from steady growth, so the policy would let a raise reach a person; the guidance says to leave it to one |
| `node-memory-pressure` | The node ran short of memory: two pods evicted, and a container killed well under its limit |
| `killed-without-oom-evidence` | Containers killed with exit code 137, but no OOMKilled reason and no metrics source: raising the limit would be a guess |
