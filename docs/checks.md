# Writing a check

A check decides one fact from the evidence: `true`, `false` or `unknown`. It is a short YAML definition with [CEL](https://cel.dev) expressions; no Go. Checks live in the [scenario packs](scenarios.md), each in its pack's `checks.yaml`, such as [`scenarios/rollout-regression/checks.yaml`](../scenarios/rollout-regression/checks.yaml), compiled into the binary. Themis has its own too, in [`internal/catalog/check/builtin.yaml`](../internal/catalog/check/builtin.yaml): `earlier_revision_healthy`, whether a healthy version before the current one is there to go back to; `failures_came_with_rollout`, whether the latest rollout brought the failures, judged by how the version before it ran, from its pods or the metrics history; `gitops_managed`, which its [GitOps protection](configuration.md#gitops) reads; `evidence_has_instructions`, for policy to read about [suspicious evidence](configuration.md#suspicious-evidence); and `rollout_recent`, for an operator's own profiles to show, which no pack runs. Every pack may run them, and none may define a check of the same name. Every check is compiled when Themis starts, and one that doesn't compile stops it with a message naming the check.

This is one of them:

```yaml
- name: oom_killed_new_revision
  version: "1"
  description: The new version runs out of memory
  topic: OOM kills in the new version
  whenFalse: The new version doesn't run out of memory
  reads: [pod_states]
  let:
    pods: pod_states.pods.filter(p, p.revision == current_revision)
    killed: >-
      pods.map(p,
        p.containers
          .filter(c, [c.state, c.lastTermination].exists(s, s != null && s.reason == "OOMKilled"))
          .map(c, p.name + "/" + c.name)
      ).flatten()
  answers:
    - when: size(killed) > 0
      answer: true
      reason: ${killed[0]} of revision ${current_revision} was OOMKilled
      cite: [ids.pod_states]
    - when: size(pods) == 0
      answer: unknown
      reason: no pods of the current revision
    - answer: false
      reason: none of the ${size(pods)} pods of revision ${current_revision} was OOMKilled
      cite: [ids.pod_states]
```

| Field | Meaning |
|---|---|
| `name` | How policy (`predicates.<name>`), profiles and investigators refer to it. Lowercase letters, digits and `_`. |
| `version` | Changes when its meaning changes, so a recorded answer says which version gave it. |
| `description` | What the check finds when it is true, in plain words. Investigators and the UI show it. |
| `whenFalse` | Optional. What the check finds when it is false, in plain words, such as "Not managed by Argo CD or Flux". The UI shows it instead of the description followed by "no". |
| `topic` | Optional. What the check is about, in a few words, such as "Peak memory use". The UI says "Peak memory use: unknown" rather than repeating the description. |
| `availability` | Optional. `true` for a check that says only whether figures other checks need are there, such as `throttling_data_available`. The UI shows it once, as where those figures come from, never as for or against a fix. |
| `reads` | The evidence templates it reads. |
| `let` | Optional named expressions the answers share. Each is worked out once, when an answer first uses it. |
| `answers` | Tried in order: the first whose `when` holds decides. The last has no `when`, and answers when none did. |
| `answers[].answer` | `true`, `false` or `unknown`. |
| `answers[].reason` | Why, in plain words. `${expression}` puts in a value: times print as RFC 3339, durations to the second. |
| `answers[].cite` | Expressions for the IDs of the evidence the answer rests on, each an ID or a list of them. |

**Event text alone never makes a check true.** Anyone who may create events in a namespace can write one that reads word for word like the kubelet's or the scheduler's. A check that decides from an event's message must also find what the event says in status that only Kubernetes' own components write in practice: a pod's container states and restart counts, its phase, or a Deployment's conditions. When the status doesn't show it, the check answers `unknown`, not `true`, and the policy treats `unknown` as not safe wherever that matters. An event may still make a check `false`, or `true` where `true` only makes the policy stricter, as `evidence_has_instructions` does.

| Check | The event | What has to show it too |
|---|---|---|
| `probe_kills` | `Killing`: failed liveness or startup probe | A container of that pod, of the current revision, restarted, and its last end, no earlier than the event's first and last kill, was a kill: exit code 137 or 143, not `OOMKilled` |
| `killed_during_startup` | `Killing`: failed startup probe | The same as `probe_kills`. Its other answer reads only the pod's status |
| `probe_timeouts` | `Unhealthy`: a probe timed out | A pod of the current revision that a failure names has a container that is running and isn't ready, or that restarted, ending no earlier than the event's first and last failure: the kubelet probes only running containers |
| `evicted_for_node_memory` | `Evicted`: the node was low on memory | The pod is still there and has failed |
| `scheduler_rejected_pods` | `FailedScheduling` | The pod is pending with no container statuses: no node took it |
| `volume_not_bound` | `FailedMount`, `FailedAttachVolume`, or `FailedScheduling` naming a volume | The pod is pending, with no container running |
| `quota_refused_pods` | `FailedCreate`: exceeded quota | The Deployment's `ReplicaFailure` condition is true, for `FailedCreate`, and names the quota |
| `failure_onset_after_rollout` | Any warning event, through `failures` | A pod of the current revision isn't ready, or a container of one restarted |
| `config_changed_before_failure` | Any warning event, through `first_failure` | The pod it names shows a failure in its status, and started no later than the event |

**Compare what the evidence shows, not a clock.** Whether a rollout brought the failures is never decided by how long ago it was: `failures_came_with_rollout` compares how long each version ran without failing, and is `unknown` when the evidence isn't there, so the policy leaves the fix to a person. A check of your own should do the same.

**Missing evidence needs no answer.** When a template an answer reads has no evidence, the check stops there as `unknown`, with a reason such as "no pod states", and the gap that explains it is attached, as for every check.

## What the expressions see

| Variable | What it is |
|---|---|
| `<template>`, such as `pod_states` | The template's latest evidence, as its JSON, except that times are timestamps and a time or object that isn't known is `null` |
| `ids.<template>` | That evidence's ID, to cite |
| `now` | When the checks run |
| `current` | The current revision from `rollout_history`, or `null` |
| `prior` | The newest revision older than `current`, or `null`, as for a Deployment's first revision: there is nothing to go back to, and its creation is no rollout |
| `rollout_at` | When `current` rolled out: its ReplicaSet's creation, or a later template change that reused it. `null` when unknown |
| `current_revision` | The current revision's number: from `rollout_history`, or without it the newest revision among the pods |
| `failures` | Signs of trouble, in order: warning events about pods, then containers that died other than `Completed`. Each has `at` (`null` when unknown), `what` and `id` |
| `first_failure` | The earliest failure, dated by the pods' status: a container's last termination, or for one that restarted more than once, a warning event about its pod that the status agrees with, one about a pod that shows a failure, dated no earlier than the pod started. Other events are left out. `null` when there is none, or one can't be dated and so could be earlier still |
| `suspicious_evidence` | Text in any evidence that looks written for the investigator, such as instructions or forged tool output, in collection order. Each has `id` and `template`, the evidence it is in, `reason` and `at`, where in it |
| `themis_fix` | Themis's own fix the target runs, from its record of an earlier incident, or `null` when it runs none: `incident`, `fix`, `done`, `appliedAt`, `replaced` (the revision it replaced), `outcome` (how its verification ended), `found` (each symptom that incident's checks found, with `check` and `reason`) and `id`, to cite. `null` is an answer, not missing evidence, so an answer that reads it needs nothing |

`current`, `prior` and `rollout_at` come from `rollout_history`, so an answer that uses them needs it. Besides standard CEL, the [lists](https://pkg.go.dev/github.com/google/cel-go/ext#Lists) and [strings](https://pkg.go.dev/github.com/google/cel-go/ext#Strings) extensions are there, such as `flatten()` and `sort()`. Each expression has a cost limit; one that goes over it, or fails, makes the check `unknown` with the error as its reason.
