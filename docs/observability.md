# Observability

## Logs

The core, the executor and the investigator log JSON to stdout, one object per line. Set the least severe level with `logLevel` in the Helm chart, or `--log-level`: `debug`, `info` (the default), `warn` or `error`.

Every line has `time`, `level`, `msg` and `component` (`core`, `executor` or `intelligence`). Messages are fixed strings, so you can search for them; values are in fields:

| Field | Meaning |
|---|---|
| `incident_id` | The incident a line is about |
| `action_id` | The action a line is about |
| `lease_id` | The investigation lease of an investigator's request, or of the investigator's own lines |
| `job` | The kind of background job that logged the line |
| `error` | The error, as text |

To follow one incident, filter on its `incident_id`. Its jobs, the investigator's lines and requests, the API calls about it and the executor's work on its action all carry it; the executor takes it from the action it loads. At `debug`, every investigator request and API call is logged with its outcome, including those refused for a missing token.

| Level | Means |
|---|---|
| `error` | Needs a human, or is a bug |
| `warn` | Degraded but handled: a retry, a refused proposal, a lost lease |
| `info` | A lifecycle milestone |
| `debug` | Everything else |

Logs never contain evidence, log tails, prompts, model output, alert annotations or secrets.

## Metrics

The core, the executor and the investigator serve Prometheus metrics on port 8082 at `/metrics`; the investigator's `--metrics-port 0` turns its server off. Labels take closed sets of values only: never an incident ID, namespace, workload name, alert name or error text. Follow one incident in the logs.

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

The investigator reports its use of the model, by `route`: the configured route, or `other`.

| Metric | Labels | Counts |
|---|---|---|
| `themis_model_requests_total` | `route`, `outcome` | Model requests: `ok`, `validation_failed`, `budget_exceeded`, `ineligible`, `deadline_exceeded`, `unavailable` |
| `themis_model_request_duration_seconds` (histogram) | `route` | Time to a request's result, repairs included |
| `themis_model_tokens_total` | `route`, `kind` | Tokens the model reported, `input` and `output` |
| `themis_model_cost_usd_total` | `route` | Cost at the route's configured price (`model.price`); a route without a price reports nothing, since a guessed price would be wrong |
| `themis_model_validation_failures_total` | `route` | Replies that did not match the schema, repaired or not |
| `themis_investigation_steps` (histogram) | | Model steps an investigation used before concluding |
| `themis_budget_exhausted_total` | `kind` | Investigations out of `tokens`, `steps`, `evidence` or `deadline` |
| `themis_core_requests_total` | `method`, `code` | Calls to the core, by gRPC method and status code |

## Traces

Every component exports traces over OTLP when `telemetry.otlp.endpoint` is set in the chart; the standard `OTEL_*` variables configure the rest, sampling included. Without an endpoint nothing is traced: no exporter runs and no connection is made.

A trace is one unit of work, not one incident: processing an alert, an investigation, an approval, executing an action, a verification check. Incidents last minutes to hours across restarts, which long traces do not survive. To find all of one incident's work, search spans for `themis.incident.id`.

- A background job runs in a trace of its own, with a link to the span that queued it. The link travels in the job's metadata; its arguments never carry trace context.
- A call from the investigator to the core, or from the core to the executor, continues the caller's trace. The investigator's idle polls for a lease are not traced.
- An investigation is one trace in the investigator, with a span per model request. Model spans carry the route, the model and the token counts, never the prompt or the reply.
- Each transition in an incident's audit trail records the trace of the work that made it, so `GET /v1/incidents/{id}` leads from a state change to its trace.
- Log lines written inside a span carry its `trace_id` and `span_id`.

Spans carry the IDs a log line would: `themis.incident.id`, `themis.lease.id`, `themis.action.id`, `themis.proposal.id` and `themis.job.kind`.
