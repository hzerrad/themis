# OOM limit too low

A Deployment's containers are killed for running out of memory: more load or more data now needs more than their memory limit. Themis checks that the kills are real, on the current version, that the latest rollout didn't bring them, and that the containers reached their limit, and may suggest raising the memory limit of the killed containers, which a person approves.

What decides whether the latest rollout brought the kills is how the version before it ran, compared with the current one (`failures_came_with_rollout`):

| The version before it | Then |
|---|---|
| Runs healthy beside the failing pods, or by Prometheus's history ran without failing for longer than the current one did before its first kill | The rollout brought them: rolling back is the rollout-regression pack's fix, and a raise is refused |
| None, as for a Deployment created minutes ago, or it failed too | A higher limit is the fix |
| Ran without failing, but the current one ran at least as long before its first kill | What changed since, such as more data or load, brought them: a higher limit is the fix |
| Its pods are gone, and no history of them: no Prometheus, or none that old | Nothing shows which: neither fix goes ahead, and a person decides |

How long ago the rollout was decides nothing.

A metrics source helps size the raise and is what tells, once the earlier version's pods are gone, whether the rollout brought the kills. With Prometheus, Themis sees each container's peak memory, can tell a container that reached its limit from one killed for something else, and reads how the earlier version's pods ran. Without one, the OOM kills in the pod states show the failure and the size comes from the current limit, but only the earlier version's pods, while they run, can show whether it was healthy.

## Alerts

`KubePodOOMKilled` and `KubePodCrashLooping`, about a Deployment. The rollout-regression pack answers them too, so an incident from them gets both packs: the investigator sees a rollback and a higher limit, with each pack's guidance, and each pack's policy decides its own fix. A rollback is right when a new version brought the kills and the one before it is healthy; a higher limit otherwise.

## Evidence

| Template | Why |
|---|---|
| `pod_states` | Which containers were OOMKilled, of which version, and how long they ran first |
| `container_memory` | Each container's memory now and at its peak over the last hour, against its limit, and which metrics source said so. Without a source, it is a gap that says why |
| `revision_health` | How the current version and the one before it ran, from Prometheus's history: restarts and OOM kills, and since when. Without history, it is a gap that says why |
| `rollout_history` | Which version runs, since when, and each version's memory limits |
| `target_events` | Restarts, and evictions when the node runs short of memory |
| `referenced_config` | Whether the configuration the pods read changed |
| `container_logs_tail` | Asked for when the pods' own words help, such as memory growing while they run |

## Checks

| Check | True when |
|---|---|
| `oom_killed_current_revision` | A container of the current version was OOMKilled |
| `earlier_revision_healthy` | Themis's own: the revision before the current one ran without failing, by its pods while they run, or with no restart or OOM kill in the metrics history once they are gone. False when there is none, or it failed; unknown with neither its pods nor a history of them |
| `failures_came_with_rollout` | Themis's own: the latest rollout brought the failures, as the table above says. False when there is no earlier version, it failed too, or the current one ran without failing at least as long; unknown when nothing shows how it ran |
| `memory_data_available` | The metrics source says each container's peak memory. False with only the Kubernetes metrics API, which keeps no history; unknown without a source |
| `memory_at_limit_before_kill` | A container peaked at 90% or more of its memory limit over the window. Unknown without peak figures |
| `oom_after_long_run` | Every OOM kill came after the container had run an hour or more |
| `evicted_for_node_memory` | A pod was evicted because its node was short of memory: an event says so, and the pod has failed. An event alone makes it `unknown` |

## Fix

`deployment.set-resources`, raising the memory limit of the killed containers, and their memory request with it if needed. The investigator decides by how much, from the evidence; its guidance suggests the peak plus 25% when there are figures, otherwise 1.5 times the current limit. Themis checks the values are valid and that the cluster can run them, never how big they are.

Themis refuses a raise unless `oom_killed_current_revision`, the check the profile names to justify it, is true: without OOM kills there is nothing to act on. Beyond that, the policy enforces, whoever suggests the fix:

| Rule | Decision | Why |
|---|---|---|
| `raise-needs-oom-kills-no-new-version-brought` | deny | Unless `failures_came_with_rollout` is false; when `memory_at_limit_before_kill` is false; or unless `evicted_for_node_memory` is false. When the rollout brought the kills, rolling back is the fix; when nothing shows whether it did, a person decides. A container that never reached its limit wasn't killed by it, and a node short of memory isn't helped by a higher limit |
| `raise-a-memory-limit-only` | deny | Unless the change raises a memory limit that exists, and does nothing else but raise a memory request: no CPU, no lower value, and no new limit where there was none |
| `raise-needs-approval` | require_approval | A person approves every raise, and its size |

There is no size cap: an operator who wants one adds a rule on `change["memory.ratio"]`.

A raise is verified by `oom_killed_current_revision`, which the profile names in `verification.checks`: it worked when no container of the new version was OOMKilled, nor restarted, and every pod stayed ready. A container is killed when it reaches its memory limit, so no kill means its peak stayed below the new limit, with or without a metrics source. A raise that didn't stop the kills, but left the workload no worse, stays, and the incident goes to a person; Themis lowers the limit again only when the raise left the workload worse.

While `oom_killed_current_revision` or `evicted_for_node_memory`, the pack's symptoms, is true and no fix was applied, Themis never closes the incident with no change on its own: it refers it to a person, saying which fix the checks support or what they found, and waits for the fix they suggest.

## Cases

| Case | What it is |
|---|---|
| `peak-at-limit-after-growth` | A ten-day-old version; a larger catalog loads at start, and containers are OOMKilled minutes later, at the 512Mi limit Prometheus shows them peak at. It ran ten days before its first kill, longer than the history shows the version before it running, so more data brought the kills. Raised by both packs' alert |
| `oom-after-rollout-history-oom` | A version rolled out an hour ago with the limit it kept is OOMKilled 40 minutes in; the older pods are gone, and Prometheus's history shows them OOMKilled at that limit too. A rollback goes back to a version that fails the same way: raising the limit is right |
| `oom-first-revision` | A Deployment created 25 minutes ago, with only revision 1, is OOMKilled at 95% of its 60Mi limit. There is nothing to roll back to, so raising the limit is right. Raised by both packs' alert |
| `oom-both-versions` | A version rolled out 30 minutes ago is OOMKilled at the limit it kept, and so are the pods of the version before it, since before the rollout. A rollback goes back to a version that fails the same way: raising the limit is right |

## Look-alikes

| Case | What it is |
|---|---|
| `oom-after-rollout` | A version rolled out 40 minutes ago lowered the limit, and is OOMKilled at it, while the version before it still runs healthy. Both packs answer: rolling back is right, and this pack's policy refuses the raise. The rollout-regression pack's `oom-new-version-healthy-before` is the same with the limit unchanged |
| `oom-after-rollout-history-healthy` | A version rolled out an hour ago with the limit it kept is OOMKilled 40 minutes in; the older pods are gone, and Prometheus's history shows them running 15 days without a restart. Rolling back is right, and the raise is refused |
| `oom-after-rollout-no-history` | The same, with only the Kubernetes metrics API, which keeps no history: nothing shows how the version before it ran, so neither fix goes ahead, and a person decides |
| `oom-kills-no-metrics` | Two workers OOMKilled minutes into a larger batch, five days after a rollout, with no metrics source: the pod states show the kills, but nothing shows how the version before it ran, so a person decides |
| `memory-grows-until-killed` | Containers climb while they run and are killed after five to six hours, again and again, with a limit that held for ten days, and logs showing the heap growing: a higher limit only delays the next kill. The checks can't tell this from steady growth, so the policy would let a raise reach a person; the guidance says to leave it to one |
| `node-memory-pressure` | The node ran short of memory: two pods evicted, and a container killed well under its limit |
| `killed-without-oom-evidence` | Containers killed with exit code 137, but no OOMKilled reason and no metrics source: raising the limit would be a guess |
