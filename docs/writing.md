# Writing for Themis

Themis writes like a judge: impartial, decisive, and always with the reason. The words are the ones an experienced SRE uses. It says what happened, why, and what follows. It does not apologise, hedge, cheer or chat.

This guide applies to everything a person reads: the web UI, messages the core writes (refusals, referrals, timeline entries), notifications, CLI output and docs written for operators.

## The voice

- **Informative.** Every message says what happened and what follows from it.
- **Precise.** Name the exact workload, revision, check, value and time.
- **Concise.** One idea per sentence. The most important word first.
- **Measured.** Calm and impersonal. Declarative sentences, active voice. No exclamation marks, no apologies, no softening.

The judge is in the manner, not the vocabulary. Terse, formal labels are good: "Findings", "Refused.", "Closed, no change", "No further action by Themis." Legal words inside sentences are not: write "reason", not "grounds"; "the checks found", not "finding" or "material"; never "ruling", "held" or "on the record". Never role-play ("Hear ye", "the court", "verdict: guilty").

## Rules

1. **State first, then what follows.** "Waiting for your approval. Roll payments back to revision 1?"
2. **Give the reason.** A refusal, referral or closure says why, in terms of the checks, the policy or the evidence. A check is named by what it finds, never by its identifier. "Refused. The rollout-regression profile allows a rollback only when the failures started after the rollout, and the checks found otherwise (there is no revision before revision 1, so no rollout came before the failures)."
3. **Say something about this incident, or cut it.** A sentence that explains how Themis thinks, such as what counts as evidence or how checks weigh it, tells the reader nothing about the incident in front of them. Delete it. "Evidence" needs no subtitle.
4. **Attribute other people's text, and leave it alone.** What the investigator or a suggester wrote is shown exactly as written, with a short attribution: "Written by the investigator's model", "Written by the suggester". No editorial next to it. This guide governs only what Themis writes.
5. **Never apologise or soften.** Not "Sorry", "unfortunately", "it seems", "we couldn't". Write "Approval expired with no decision.", not "Sorry, this approval has expired."
6. **Errors name the cause and the remedy.** "Events: access denied in shop. Grant read access in the Role Themis has in shop." Say the remedy once, however many things it fixes.
7. **Name actions by what they do, and keep the name.** The button "Approve the rollback" produces "Rollback approved."
8. **One name per state, everywhere.** See the states below.
9. **Exact numbers and times.** "3 pods", "13 Nov, 11:05", "2 of 3 pods are ready", "within 5m". Never "a few", "soon", "some".
10. **Themis never speaks as "I" or "we".** It is not a person. The investigator's own text may, and is labelled.
11. **Sentence case** for headings, buttons and labels. No all-caps labels.
12. **What Themis sends the model stays as it is.** Check descriptions and reasons, evidence notes, suspicion reasons and the refusals of a suggestion reach the investigator, and the recorded evaluation replays them word for word. Where people read one of those texts, the UI shows its own wording instead of changing what the model sees.

## Machine tells to avoid

These make a message read as generated. Each has a plainer form.

| Tell | Before | After |
|---|---|---|
| Colon chains | Refused: the check that justifies a rollback isn't true: failure_onset_after_rollout is false. | Refused. The rollout-regression profile allows a rollback only when the failures started after the rollout, and the checks found otherwise (…). |
| Check identifiers in prose | No fix proposed. The checks support a rollback (failure_onset_after_rollout is true). | No fix proposed. The checks support a rollback (the failures started after the rollout). |
| Semicolons gluing clauses | Not read; all 6 evidence requests were used. | Not read. The investigation used all 6 of its evidence requests. |
| Legal jargon in prose | It is material, not a finding; the checks do not rely on it. | Themis's checks don't read log text, so it had no effect on them. |
| "X, not Y" as a flourish | Recommended, not applied: Argo CD manages this workload. | Argo CD manages this workload. Apply the change in Git. |
| "It is…" and "There is…" openings | It is unknown whether the change was made. | Unknown whether the change was made. The executor stopped while applying it. |
| Abstract nouns where a verb works | Verification was inconclusive due to a modification outside Themis. | Inconclusive. The workload was changed outside Themis during verification. |
| Explaining Themis to the reader | Material read from the cluster. It is not a finding until a check weighs it. | (deleted) |
| Editorial next to someone's text | Written by the model, not checked by Themis | Written by the investigator's model |

One colon that introduces a label, a quotation, a list or an error from elsewhere is fine: "Container memory: no metrics source.", "Kubernetes rejected the change: admission webhook denied the request."

## Suspicious text in evidence

Logs, events and config are written by whoever runs the workload, so they can hold text aimed at Themis. The incident page shows one block per flagged text:

1. A title naming the evidence: "Suspicious text in the container logs" (or the events, the pod states, the referenced config, the rollout history).
2. The text itself, in its own monospace block. Viewers, who may not read raw evidence, see "Responders can read the text." instead.
3. Where it is: "Pod payments-76fd5bcb57-bdf66, container app, previous run", or "BackOff event on Pod/payments-76fd5bcb57-bdf66".
4. Three plain sentences: what it is, whether it mattered, what to do.

> This line is written as an instruction to Themis. Themis's checks don't read log text, so it had no effect on them. Approve only if the checks below justify the rollback on their own.

When checks of the incident read that kind of evidence, the second sentence names them and the third asks to confirm their results in the cluster: "Checks that read events may reflect it: when the failures started and restarts for failing probes. Approve only after confirming their results in the cluster." Past three blocks, the rest fold away. Escalation notices carry the same facts in one line, without the text.

## States

| State | Use | Not |
|---|---|---|
| Investigating | Evidence is being collected and weighed | analysing, working on it |
| Waiting for your approval | A fix awaits a person | needs approval, pending |
| Applying | The approved change is being made | executing, in progress |
| Verifying | Themis is checking the change worked | watching, monitoring |
| Fixed | The change worked and held | resolved successfully |
| Restored | The change didn't help and was undone | rolled back (ambiguous with the fix) |
| Referred to a person | Themis hands the incident over | escalated, needs a human |
| Waiting for a suggested fix | The incident waits for a fix a person or a tool suggests: after a denial or a refusal, or referred with no fix applied | pending, waiting for input |
| Closed, no change | Nothing was applied | closed without acting, nothing changed |
| Recommended | A change Themis can't apply here, which a person makes: the executor is off or may not apply it, or GitOps manages the workload | suggested |
| Operational | A part of the install works (Status) | working, OK, healthy |
| Action required | A part of the install needs a change by a person (Status) | needs attention, warning |
| Failing | A part of the install doesn't work (Status) | not working, broken, down |

## Words

| Use | Avoid | Why |
|---|---|---|
| Findings, as a heading; "the checks found", in a sentence | finding, in a sentence | The label is terse; the sentence says what was found |
| reason | grounds | The plain word |
| refused | rejected, failed, blocked | Themis refuses; it doesn't fail |
| supports / against / unknown | true / false / n/a, in prose | What a check's answer means for the decision |
| suspicious text | material, text addressed to Themis | Says what the reader is looking at |
| the investigator | the AI, the bot, the LLM | Its role, not its technology |
| the investigator's note | AI summary | Labelled as written by its model |
| break-glass token | emergency token | The standard operations term |
| fix | remediation, capability (in UI) | The plain word |
| person | human, user | |

## Examples

| Before | After |
|---|---|
| A fix needs your approval. Check what it changes, then approve or deny it. | Waiting for your approval. Roll payments back to revision 1? |
| Grounds for denying (optional) | Reason for denying (optional) |
| Closed without acting: no fix was justified or allowed. | Closed, no change. No fix met the checks and policy. |
| Fixed. The workload recovered and stayed healthy. (while it is still throttled) | Not fixed. Since the change, the checks found that a container is throttled at its CPU limit (app was throttled in 57% of its CPU periods over 7m, at its limit of 0.3 CPU). |
| No fix proposed. The evidence doesn't point to one cause. (on an incident a person closed) | Closed by alice. Reason: handled by the vendor. |
| Partly recovered: 2 of 3 pods ready. Referred to a person. | Partly recovered. 2 of 3 pods are ready. Referred to a person. |
| The fix didn't help, so Themis is putting the workload back as it was before it acted. | The rollback didn't help. Restoring the previous state. |
| Not restored: the workload changed after Themis's fix. It stays as it is. | Not restored. The workload changed after Themis's fix, so it stays as it is. |
| Approval expired: no one approved the fix in time. | Approval expired with no decision. Referred to a person. |
| Some logs contain text addressed to Themis. It is material, not a finding; the checks do not rely on it. | The block under "Suspicious text in evidence", above. |
| No incidents on record. | No incidents. One opens when Alertmanager sends an alert about a workload Themis watches. |
| No fix proposed. Finding: an image can't be pulled (inventory-…/app, …). No fix applies to this kind of incident. | No fix proposed. The checks found that an image can't be pulled (inventory-…/app, …). Themis has no fix for this kind of incident. Referred to a person. |
| Investigation failed after 3 attempts: the model was unreachable. | Investigation failed after 3 attempts. Last error: the model was unreachable. Referred to a person. |
| 6 findings support the rollback. | 6 checks support the rollback. None is against it. 1 is unknown. |
| Managed by GitOps (Argo CD or Flux): No | ✓ Not managed by Argo CD or Flux |
| A metrics source says how much memory the containers used at their peak — Unknown | ? Peak memory use: unknown — no container memory |
| Metrics: The Kubernetes metrics API isn't served in this cluster: install metrics-server, or set a Prometheus address. Fix: Install metrics-server, or set a Prometheus address. | Metrics · Action required · No metrics source. Install metrics-server, or set metrics.prometheus.address. |
| Containers' memory use couldn't be read: collect container_memory: no figures: the Kubernetes metrics API isn't served in this cluster | Container memory and container CPU: no metrics source. Install metrics-server, or set metrics.prometheus.address. |
| Policy rule rollback-needs-approval requires a person's approval. It is the rollout-regression profile's policy. | Policy rule rollback-needs-approval of the rollout-regression profile requires a person's approval. |
