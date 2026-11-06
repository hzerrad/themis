# Writing for Themis

Themis speaks like a judge: impartial, measured, and grounded in the record. It states what it found, on what grounds, and what follows. It does not apologise, hedge, cheer or chat.

This guide applies to everything a person reads: the web UI, messages the core writes (refusals, escalations, timeline entries), notifications, CLI output and docs written for operators.

## The voice

- **Informative.** Every message says what happened and what follows from it.
- **Precise.** Name the exact workload, revision, check, value and time.
- **Concise.** One idea per sentence. The most important word first.
- **Measured.** Calm and impersonal. Declarative sentences, active voice. No exclamation marks, no apologies, no softening.

A judge's manner, not a courtroom costume. Use a legal word only when it is the most precise word for the thing: *finding*, *grounds*, *refused*, *ruling*, *record*. Never role-play ("Hear ye", "the court", "verdict: guilty").

## Rules

1. **State first, then what follows.** "Waiting for your approval. Roll payments back to revision 1?"
2. **Give grounds.** A refusal, escalation or closure says why, in terms of the checks, policy or evidence. "Refused: none of the checks that justify a probe change is true."
3. **Distinguish findings from claims.** Themis's checks are findings. The investigator's text is a claim, labelled as written by the model. Text from the cluster (logs, events) is material, never a finding.
4. **Never apologise or soften.** Not "Sorry", "unfortunately", "it seems", "we couldn't". Write "Approval expired at 11:05.", not "Sorry, this approval has expired."
5. **Errors name the cause and the remedy.** "The events couldn't be read: Themis isn't allowed to read them in shop. Grant it in the Role Themis has in shop."
6. **Name actions by what they do, and keep the name.** The button "Approve the rollback" produces "Rollback approved."
7. **One name per state, everywhere.** See the states below.
8. **Exact numbers and times.** "3 pods", "13 Nov, 11:05", "2 of 3 pods ready". Never "a few", "soon", "some".
9. **Themis never speaks as "I" or "we".** It is not a person. The investigator's own text may, and is labelled.
10. **Sentence case** for headings, buttons and labels. No all-caps labels.

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
| Closed, no change | Nothing was applied | closed without acting, nothing changed |
| Recommended | A change Themis may not apply itself (GitOps) | suggested |
| Operational | A part of the install works (Status) | working, OK, healthy |
| Action required | A part of the install needs a change by a person (Status) | needs attention, warning |
| Failing | A part of the install doesn't work (Status) | not working, broken, down |

## Words

| Use | Avoid | Why |
|---|---|---|
| finding | result, output | A check's answer is a finding of fact |
| grounds | reason (in refusals) | Refusals rest on grounds |
| refused | rejected, failed, blocked | Themis refuses; it doesn't fail |
| supports / against / unknown | true / false / n/a, in prose | What a finding means for the decision |
| the investigator | the AI, the bot, the LLM | Its role, not its technology |
| the investigator's note | AI summary | It is a claim, not a finding |
| break-glass token | emergency token | The standard operations term |
| fix | remediation, capability (in UI) | The plain word |
| person | human, user | |

## Examples

| Before | After |
|---|---|
| A fix needs your approval. Check what it changes, then approve or deny it. | Waiting for your approval. Roll payments back to revision 1? |
| Closed without acting: no fix was justified or allowed. | Closed, no change. No fix met the checks and policy. |
| The fix helped but the workload is not fully healthy. A human should take over. | Partly recovered: 2 of 3 pods ready. Referred to a person. |
| The fix didn't help, so Themis is putting the workload back as it was before it acted. | The rollback didn't help. Restoring the previous state. |
| It cited evidence Themis never collected. | Refused: it cites evidence Themis didn't collect. |
| Approving approves exactly this change. If the workload changes first, the approval no longer applies. | Applies exactly this change. Void if payments changes before it is applied. |
| Some evidence contains text that looks like instructions to Themis. | Some logs contain text addressed to Themis. It is material, not a finding; the checks do not rely on it. |
| Something it depends on is failing | A dependency is failing |
| Already connected Alertmanager? This step shows done when the first alert arrives. | Done when Themis receives its first alert. |
| Managed by GitOps (Argo CD or Flux): No | ✓ Not managed by Argo CD or Flux |
| A metrics source says how much memory the containers used at their peak — Unknown | ? Peak memory use: unknown — no container memory |
| Metrics: The Kubernetes metrics API isn't served in this cluster: install metrics-server, or set a Prometheus address. Fix: Install metrics-server, or set a Prometheus address. | Metrics · Action required · No metrics source. Install metrics-server, or set metrics.prometheus.address. |
| No incidents yet. | No incidents on record. One opens when Alertmanager sends an alert about a workload Themis watches. |
