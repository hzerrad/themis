# Writing a check

A check decides one fact from the evidence: `true`, `false` or `unknown`. It is a short YAML definition with [CEL](https://cel.dev) expressions; no Go. Checks live in the [scenario packs](scenarios.md), each in its pack's `checks.yaml`, such as [`scenarios/rollout-regression/checks.yaml`](../scenarios/rollout-regression/checks.yaml), compiled into the binary. Every check is compiled when Themis starts, and one that doesn't compile stops it with a message naming the check.

This is one of them:

```yaml
- name: oom_killed_new_revision
  version: "1"
  description: The new version runs out of memory
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
| `reads` | The evidence templates it reads. |
| `let` | Optional named expressions the answers share. Each is worked out once, when an answer first uses it. |
| `answers` | Tried in order: the first whose `when` holds decides. The last has no `when`, and answers when none did. |
| `answers[].answer` | `true`, `false` or `unknown`. |
| `answers[].reason` | Why, in plain words. `${expression}` puts in a value: times print as RFC 3339, durations to the second. |
| `answers[].cite` | Expressions for the IDs of the evidence the answer rests on, each an ID or a list of them. |

**Missing evidence needs no answer.** When a template an answer reads has no evidence, the check stops there as `unknown`, with a reason such as "no pod states", and the gap that explains it is attached, as for every check.

## What the expressions see

| Variable | What it is |
|---|---|
| `<template>`, such as `pod_states` | The template's latest evidence, as its JSON, except that times are timestamps and a time or object that isn't known is `null` |
| `ids.<template>` | That evidence's ID, to cite |
| `now` | When the checks run |
| `current` | The current revision from `rollout_history`, or `null` |
| `prior` | The newest revision older than `current`, or `null` |
| `rollout_at` | When `current` rolled out: its ReplicaSet's creation, or a later template change that reused it. `null` when unknown |
| `current_revision` | The current revision's number: from `rollout_history`, or without it the newest revision among the pods |
| `failures` | Signs of trouble, in order: warning events about pods, then containers that died other than `Completed`. Each has `at` (`null` when unknown), `what` and `id` |
| `first_failure` | The earliest failure, or `null` when there is none or one has no time, and so could be earlier still |

`current`, `prior` and `rollout_at` come from `rollout_history`, so an answer that uses them needs it. Besides standard CEL, the [lists](https://pkg.go.dev/github.com/google/cel-go/ext#Lists) and [strings](https://pkg.go.dev/github.com/google/cel-go/ext#Strings) extensions are there, such as `flatten()` and `sort()`. Each expression has a cost limit; one that goes over it, or fails, makes the check `unknown` with the error as its reason.
