# Probe restart storm

A Deployment's probes fail a container that works, only slower than they allow: its liveness or startup probe restarts it before it finishes starting, or its probes time out on answers that come a little late, so it is restarted again and again, or kept out of service. Themis checks that probes are what fail it and that the application isn't failing by itself, and may suggest changing the probes' timing, which a person approves.

## Alerts

`KubePodCrashLooping` and `KubePodNotReady`, about a Deployment.

Both are answered by another pack as well: `KubePodCrashLooping` by rollout-regression, and `KubePodNotReady` by stuck-pods. The incident then gets the packs combined, shown under the one with the higher priority (this pack's is 60): the investigator sees every pack's checks and guidance, and chooses between a rollback, a probe change or neither from the evidence. Each pack's policy decides only its own fix.

## Evidence

| Template | Why |
|---|---|
| `rollout_history` | Which revision runs and since when, and each revision's probe timings: delay, period, timeout and failures allowed |
| `pod_states` | How often containers restart, how long each last ran, and how it ended: stopped, or exited by itself |
| `target_events` | What the kubelet said: which probe failed and how (timed out, refused, or an error answer), and which containers it restarted for it |
| `container_logs_tail` | Asked for to tell an application that is starting or serving from one that is failing |

## Checks

| Check | True when |
|---|---|
| `probe_kills` | An event says the kubelet restarted a container for failing its liveness or startup probe, and a container of that pod of the current revision was killed (exit code 137 or 143) and restarted since. An event alone makes it `unknown` |
| `probe_timeouts` | Every probe failure an event reports ran out of time, rather than getting an error answer or a refused connection, and a pod of the current revision the failures name has a container that isn't ready or restarted since. Events alone make it `unknown` |
| `killed_during_startup` | A container failed its startup probe and was killed and restarted, as `probe_kills` requires, or one of the current revision, without a startup probe, was stopped no later than its liveness probe could first fail it: its initial delay, its failures allowed and one more period, plus the default 30 seconds to stop. It never passed the probe |
| `crashed_on_its_own` | A container exited with a code other than 0, 137 or 143, the codes of a container the kubelet stops, or ran out of memory |
| `rollout_recent` | The current revision rolled out within 6 hours |

Whether the logs show errors, whether a probe's error answer means the application is unhealthy or only not ready yet, and whether a dependency is down are left to the investigator: they can't be read reliably from the evidence by a check. A container that traps SIGTERM and exits with 1 looks crashed to `crashed_on_its_own`.

## Fix

`deployment.set-probes`, changing the timing of the container's liveness, readiness or startup probe: the investigator decides which probes and which values, from the evidence, and cites it. The fix never adds or removes a probe, or changes what it calls. Themis sets no limit on the values: the approver sees each field before and after.

Themis refuses any fix on a Deployment GitOps owns, whatever the pack. Besides that, the policy enforces, whoever suggests the fix:

| Rule | Decision | Why |
|---|---|---|
| `probe-change-needs-probe-failures` | deny | Unless `probe_kills` or `probe_timeouts` is true: nothing shows the probes failing the container |
| `probe-change-cannot-fix-a-crash` | deny | Unless `crashed_on_its_own` is false: a container that exits with an error or runs out of memory fails by itself |
| `lenient-readiness-keeps-traffic-on-failing-pods` | require_approval | A more lenient readiness probe keeps traffic going to pods that fail it for longer; the rule's name says so to the approver |
| `probe-change-needs-approval` | require_approval | A person approves every probe change |

Putting back what Themis changed, after a change that didn't help, passes.

## Cases

| Case | What it is |
|---|---|
| `slow-start-killed-by-liveness` | After a node drain, the ledger takes about 40 seconds to load its index before it listens, and its liveness probe kills it at 30, every time; a longer initial delay is right |
| `liveness-timeout-under-load` | Under the afternoon peak, search-api answers everything, its probes included, in 1.2 to 1.6 seconds, and its 1-second probe timeouts restart it and keep it out of service; longer timeouts are right, the readiness one called out to the approver |

## Look-alikes

| Case | What it is |
|---|---|
| `crash-with-failing-liveness` | The worker panics on a bad message, exit code 1, stack trace in the logs; the liveness probe failing is a symptom. The policy refuses a probe change |
| `new-version-hangs-on-start` | A new revision blocks on start and is killed by its liveness probe, while the revision before it starts in seconds with the same probes. With `KubePodCrashLooping`, rollout-regression is combined in, and a rollback is right, not a probe change. The probe policy doesn't refuse one here: the logs, not a check, tell this from a slow start |
| `dependency-down-fails-readiness` | The database is down, and every pod's readiness probe answers 503, as designed. With `KubePodNotReady`, stuck-pods is combined in. A more lenient readiness probe would send traffic to pods that can't serve it; the policy refuses it |

Not covered yet: readiness that times out because the container is starved of CPU, where raising the CPU limit, the cpu-throttling pack's fix, is right and a longer timeout only hides it.
