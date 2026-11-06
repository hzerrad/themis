# Themis

Themis turns Kubernetes alerts into incidents, works out what went wrong, and suggests a fix that a person can approve, then checks whether it worked. These are the words it uses for that.

## Incidents

**Incident**:
One problem with one workload, opened from the alerts about it and followed until it ends.
_Avoid_: alert, ticket, case

**Target**:
The exact object an incident is about, down to its identity in Kubernetes: a workload deleted and recreated under the same name is a different target.
_Avoid_: resource, subject

**Workload**:
The Kubernetes object a target names, as Themis reads it: its pod template, the revisions it keeps, the pods it runs and their state. Today only Deployments.
_Avoid_: app, service, deployment (unless the kind is meant)

**Profile**:
A named set of rules for a group of workloads: which evidence Themis may read, which checks it runs, which capabilities it may suggest, and which policy decides. Each scenario pack brings one; an install can add its own, or replace a pack's with one of the same name. An incident gets every profile its alert matches, combined; the highest-priority one is its primary profile, the one it is shown under.
_Avoid_: config, ruleset

**Scenario pack**:
One kind of incident Themis handles, as a folder: the alerts it answers, the evidence it reads, the checks it runs, the root causes it tells apart, the fix it may suggest, its policy, and the cases that prove it.
_Avoid_: plugin, playbook, runbook

**Poll mode, push mode**:
How alerts reach Themis. In poll mode, Themis reads the active alerts from Alertmanager's API. In push mode, Alertmanager sends them to Themis's webhook. An alert that arrives both ways is the same alert.
_Avoid_: pull, reading, intake mode

**Supersede**:
To close an incident because its target changed or was replaced, and open a new one for the target as it is now, linked to the old.
_Avoid_: merge, reopen

**Escalate**:
To hand an incident to a person, with what Themis knows, because Themis won't or can't go further. When Themis applied no fix, the incident then waits for the fix the person suggests; Themis never closes it with no change on its own while a symptom is true.
_Avoid_: page, alert

## Investigation

**Evidence**:
A fact Themis read about a target, such as its pods' states, its events or its recent logs, kept with the incident so a decision can cite it.
_Avoid_: data, context, signals

**Suspicious evidence**:
Evidence holding text that looks written for the investigator rather than by the workload, such as instructions, forged tool output or an order to make a fix. Themis flags it and warns whoever approves a fix; the text stays in the evidence.
_Avoid_: injection, poisoned evidence

**Evidence template**:
A named, read-only question about a target whose answer is evidence.
_Avoid_: collector, query, tool

**Metrics source**:
Where Themis reads how much memory and CPU a target's containers use: the Kubernetes metrics API, which knows only the present, or a server answering Prometheus queries, which also knows peaks and throttling.
_Avoid_: datasource, backend, provider

**Gap**:
Evidence an investigation needed and couldn't have, with why: not allowed, gone, too old, or there was nothing to find.
_Avoid_: missing data, error

**Hint**:
Plain advice to the operator on how to close a gap, such as a permission to grant.
_Avoid_: remedy, fix

**Check**:
A fact Themis decides from evidence, true, false or unknown, such as "the crash started with the latest rollout". It is unknown when the evidence it reads is missing or doesn't settle it, never guessed.
_Avoid_: predicate, rule, signal

**Symptom**:
A check a profile names because it finds the failure itself, such as OOM kills or throttling at the CPU limit, rather than whether a fix is right.
_Avoid_: signal, indicator

**Root cause**:
Why an incident happened, named from a short list its scenario pack gives, such as "the new version's memory limit is too low" or "the evidence can't tell". Lists make answers comparable.
_Avoid_: diagnosis, reason, category

**Catalog**:
Everything this install of Themis can read, check and change: its fixes, evidence templates and checks.
_Avoid_: registry (one kind of lookup inside it), plugins

**Investigator**:
What reads the evidence and checks and suggests a fix or declines to: Themis's own model-driven one, or a tool outside Themis.
_Avoid_: agent, AI, model

**Suggestion**:
A fix put forward for an incident, by an investigator or a person, citing the evidence it rests on. Themis judges it before anything happens.
_Avoid_: recommendation, proposal

## Fixing

**Capability**:
A kind of change Themis knows how to make, such as rolling a Deployment back to an earlier revision. "Fix" is the plain word for it in what operators read.
_Avoid_: remediation, action type, playbook

**Action**:
One concrete change to one target, derived from the live cluster rather than from the suggestion's words, and bound to exactly what was approved.
_Avoid_: operation, job, change request

**Policy**:
The rules that decide, for each action, whether it is forbidden or needs a person's approval.
_Avoid_: permissions, RBAC

**Justifying check**:
A check a profile names for one of its fixes: Themis refuses the fix unless one of them is true, whoever suggests it, since evidence anyone can write may suggest a fix but never justify one.
_Avoid_: precondition, guard

**Core protection**:
A rule Themis applies to every action before any policy, whichever profile offers the fix and whoever suggested it, and that no policy can loosen: Themis refuses a fix none of its justifying checks supports, and never changes a workload GitOps manages, or may manage, only recommending the fix, for a person to apply in Git.
_Avoid_: default policy, built-in rule

**Approval**:
A person's yes to one action, valid only for that action as it was when approved and only for a while.
_Avoid_: sign-off, consent

**Denial**:
A person's no to one action, with their reason as they wrote it. The incident then waits for another fix, one suggested or one found by investigating it again, unless the person closes it.
_Avoid_: rejection, veto

**Verification**:
Watching a target for a while after an action to tell whether the incident is resolved, contained, unchanged or worse.
_Avoid_: health check, validation

**Restore**:
Putting a target back as it was before Themis acted, after a fix that didn't help, so a person finds it as they left it.
_Avoid_: undo, revert, rollback (rollback is a capability)
