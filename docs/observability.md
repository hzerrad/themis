# Observability

## Logs

The core and the executor log JSON to stdout, one object per line. Set the least severe level with `logLevel` in the Helm chart, or `--log-level`: `debug`, `info` (the default), `warn` or `error`.

Every line has `time`, `level`, `msg` and `component` (`core` or `executor`). Messages are fixed strings, so you can search for them; values are in fields:

| Field | Meaning |
|---|---|
| `incident_id` | The incident a line is about |
| `action_id` | The action a line is about |
| `lease_id` | The investigation lease of an investigator's request |
| `job` | The kind of background job that logged the line |
| `error` | The error, as text |

To follow one incident, filter on its `incident_id`. Its jobs, the investigator's requests, the API calls about it and the executor's work on its action all carry it; the executor takes it from the action it loads. At `debug`, every investigator request and API call is logged with its outcome, including those refused for a missing token.

| Level | Means |
|---|---|
| `error` | Needs a human, or is a bug |
| `warn` | Degraded but handled: a retry, a refused proposal, a lost lease |
| `info` | A lifecycle milestone |
| `debug` | Everything else |

Logs never contain evidence, log tails, prompts, model output, alert annotations or secrets.

## Metrics

The core and the executor serve Prometheus metrics on port 8082 at `/metrics`. Labels take closed sets of values only: never an incident ID, namespace, workload name, alert name or error text. Follow one incident in the logs.

Counters count what was committed: a change the database rolled back is never counted. Each event is counted by the process that decided it, so sum a counter across the core and the executor. `themis_incident_transitions_total` and `themis_escalations_total` have series in both.

| Metric | Labels | Counts |
|---|---|---|
| `themis_incident_transitions_total` | `from`, `to` | Incident state changes |
| `themis_escalations_total` | `from` | Incidents handed to humans |
| `themis_stage_duration_seconds` (histogram) | `stage` | Time to a proposal (`to_proposal`) and to the end (`to_close`), and in `approval_wait`, `execution`, `verification` |
| `themis_investigations_total` | `outcome` | Investigation leases ended: `proposed`, `abstained`, `failed`, `budget_exhausted`, `lease_lost` |
| `themis_proposals_total` | `result` | Proposals `accepted` or `rejected` |
| `themis_safety_events_total` | `kind` | Proposals naming another target (`wrong_target`), a capability the profile does not allow (`unknown_capability`) or uncollected evidence (`unknown_evidence`) |
| `themis_policy_decisions_total` | `decision` | `allow`, `require_approval`, `deny`, or `error` when a rule failed, which denies |
| `themis_gitops_refusals_total` | | Actions denied on a GitOps-managed target |
| `themis_approvals_total` | `result` | `approved`, `denied`, `expired`, and refused attempts: `digest_mismatch`, `replayed` |
| `themis_actions_total` (executor) | `capability`, `result` | `succeeded`, `stale`, `dry_run_failed`, `failed`, `ambiguous` |
| `themis_verification_outcomes_total` | `outcome` | `resolved`, `contained`, `unchanged`, `worsened`, `inconclusive` |
| `themis_notifications_total` | `result` | Webhook posts `delivered` or `failed` |
| `themis_evidence_collected_total`, `themis_evidence_truncated_total`, `themis_redactions_total` | `template` | Evidence items, those cut to their bound, and secrets replaced |

What waits now is read from the database at each scrape, with one query and a timeout. Every core replica reports the same value, so aggregate these with `max`, not `sum`:

| Metric | Labels | Value |
|---|---|---|
| `themis_incidents` | `state` | Incidents in each state |
| `themis_approvals_pending` | | Actions awaiting approval |
| `themis_oldest_pending_approval_age_seconds` | | Age of the oldest one, or 0 |
| `themis_metrics_collection_errors_total` | | Scrapes whose query failed; they report none of the three above |

The intake's `themis_intake_*` metrics count alerts received and processed.
