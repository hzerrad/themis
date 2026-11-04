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
A named set of rules for a group of workloads: which evidence Themis may read, which capabilities it may suggest, and which policy decides.
_Avoid_: config, ruleset

**Supersede**:
To close an incident because its target changed or was replaced, and open a new one for the target as it is now, linked to the old.
_Avoid_: merge, reopen

**Escalate**:
To hand an incident to a person, with what Themis knows, because Themis won't or can't go further.
_Avoid_: page, alert

## Investigation

**Evidence**:
A fact Themis read about a target, such as its pods' states, its events or its recent logs, kept with the incident so a decision can cite it.
_Avoid_: data, context, signals

**Evidence template**:
A named, read-only question about a target whose answer is evidence.
_Avoid_: collector, query, tool

**Gap**:
Evidence an investigation needed and couldn't have, with why: not allowed, gone, too old, or there was nothing to find.
_Avoid_: missing data, error

**Hint**:
Plain advice to the operator on how to close a gap, such as a permission to grant.
_Avoid_: remedy, fix

**Check**:
A fact Themis decides from evidence, true, false or unknown, such as "the crash started with the latest rollout".
_Avoid_: predicate, rule, signal

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

**Approval**:
A person's yes to one action, valid only for that action as it was when approved and only for a while.
_Avoid_: sign-off, consent

**Verification**:
Watching a target for a while after an action to tell whether the incident is resolved, contained, unchanged or worse.
_Avoid_: health check, validation

**Restore**:
Putting a target back as it was before Themis acted, after a fix that didn't help, so a person finds it as they left it.
_Avoid_: undo, revert, rollback (rollback is a capability)
