# Observability

Themis's three components, the core, the executor and the investigator (`intelligence`), write JSON logs, serve Prometheus metrics, and export OpenTelemetry traces when you give them a collector. All three use the same field names, so one incident can be followed across them: filter logs on `incident_id`, search spans for `themis.incident.id`. Metrics never name an incident; they say how many and how long.

None of it carries workload data. Logs, metric labels and spans never contain evidence, container logs, prompts, model output, alert annotation values, policy inputs beyond their names, tokens or secrets. The end-to-end test plants canaries in a secret, a container's logs, a prompt injection in those logs and an alert annotation, and fails if one reaches any component's logs or metrics.

## Status

The UI's **Status** page, and `GET /v1/status` for anyone with the viewer role, check each part of an install when asked. Each part is **Operational**, **Action required** or **Failing** (or **Waiting** or **Off**), with a short finding and, where something must change, the remedy:

| Part | Operational when | Otherwise |
|---|---|---|
| Database | It answers | Nothing else can be checked |
| Investigator | It asked for work in the last 2 minutes | It never has, or stopped |
| Model | The last investigation got answers | Why the last one failed: unreachable, answers in the wrong format, too slow, or out of token budget; or "waiting" until the first incident |
| Alerts | Themis reads Alertmanager, at which address and how often, and when it last did; or, without reading, Alertmanager sent alerts, and through which receiver | Not read yet, why the last read failed (unreachable, credentials refused, not Alertmanager's answer), reads stopped, no address; for the webhook, none yet, the wrong intake token, or refused while no admin account existed |
| Alert rules | Prometheus has a rule for every alert the profiles answer: "All 7 alerts have rules in Prometheus." | Which are missing, with the rules to add on the Setup page; not checked, without a Prometheus to ask, with the alerts to check by hand; or Prometheus doesn't answer, with its address |
| Namespaces | Themis may read pods, deployments and replica sets in each one it watches, and events and logs | What it can't read, and where |
| Metrics | A metrics source answers: Prometheus, or the Kubernetes metrics API | No metrics source, or one failing; without Prometheus, no history of peak memory or CPU throttling |
| Fixes | The executor is installed and reachable | Off, or unreachable |
| Sign-in | People sign in with accounts or an identity provider | The initial admin password is not retired, or only the break-glass token signs in |

The investigator and Alertmanager's webhook are recorded as heard from at most every 30 seconds, and every read of Alertmanager as it happens, in the database, so every core replica sees the same.

## Logs

Each component logs to stdout, one JSON object per line. Set the least severe level with `logLevel` in the Helm chart, or `--log-level`: `debug`, `info` (the default), `warn` or `error`.

Every line has `time`, `level`, `msg` and `component` (`core`, `executor` or `intelligence`). Messages are fixed strings you can search for; values are in fields:

| Field | Meaning |
|---|---|
| `incident_id` | The incident a line is about |
| `action_id` | The action a line is about |
| `lease_id` | The investigation lease of an investigator's request, or of the investigator's own lines |
| `job` | The kind of background job that logged the line |
| `trace_id`, `span_id` | The span a line was written in, when tracing is on |
| `error` | The error, as text |

To follow one incident, filter on its `incident_id`. Its background jobs, the investigator's lines and requests, the API calls about it and the executor's work on its action all carry it. Each state change is one `info` line, `incident transitioned`, with `from`, `to` and `actor`. Its reason is in the audit trail (`GET /v1/incidents/{id}`), not the log, since an investigation's summary, written by the model, can be one.

| Level | Means |
|---|---|
| `error` | Needs a human, or is a bug |
| `warn` | Degraded but handled: a retry, a refused proposal, a lost lease |
| `info` | A lifecycle milestone |
| `debug` | Everything else. At `debug`, every investigator request and API call is logged with its outcome, refused ones included |

## Metrics

Each component serves `/metrics` on port 8082, behind a Service of the same name: `<release>-core`, `<release>-executor` and `<release>-intelligence`, port `metrics`. The investigator's `--metrics-port 0` turns its server off.

Labels take closed sets of values: never an incident ID, namespace, workload name, alert name or error text, and no label takes more than 20 values. Except the intake's, whose labels depend on each request, every series exists from startup at zero, so `rate()` and `increase()` work before the first event.

### Counters

A counter counts what was committed: a change the database rolled back is never counted. Each event is counted by the process that decided it, so sum across the core and the executor. The executor moves incidents through `executing`, so `themis_incident_transitions_total` and `themis_escalations_total` have series in both.

| Metric | Labels | Counts |
|---|---|---|
| `themis_incident_transitions_total` | `from`, `to` | Incident state changes |
| `themis_escalations_total` | `from` | Incidents handed to humans |
| `themis_stage_duration_seconds` (histogram) | `stage` | Time from an incident's start to a proposal (`to_proposal`) and to its end (`to_close`), and time spent in `approval_wait`, `execution` and `verification` |
| `themis_investigations_total` | `outcome` | Investigation leases ended: `proposed`, `abstained`, `failed`, `budget_exhausted`, or `lease_lost` when the investigator stopped renewing |
| `themis_proposals_total` | `result` | Proposals `accepted` or `rejected` |
| `themis_external_proposals_total` | `result` | Of those, the ones from outside Themis |
| `themis_token_reviews_total` | `result` | Agents' tokens checked with the Kubernetes API server: `accepted`, `refused`, or `failed` when it couldn't answer |
| `themis_token_review_cache_hits_total` | | Agents' tokens answered from a recent check instead |
| `themis_requests_limited_total` | `kind` | Agents' calls refused for coming too often: `address` for tokens tried from one address, `reads`, `suggestions` |
| `themis_abstentions_total` | `kind` | Investigations that couldn't decide: `blind` when evidence Themis could have had was missing, `ambiguous` when it saw everything and found no clear answer. Blind ones should be rare on a well-configured install |
| `themis_missing_evidence_total` | `template`, `cause` | Evidence an investigation needed and couldn't have, counted once per lease: `forbidden`, `not_in_profile`, `expired`, `not_found`, `budget_spent`, `data_ceiling`, `timeout`, `unavailable`, or `absent` when there was nothing |
| `themis_safety_events_total` | `kind` | Proposals naming another target (`wrong_target`), a capability the profile does not allow (`unknown_capability`), or evidence that was never collected (`unknown_evidence`) |
| `themis_policy_decisions_total` | `decision` | `allow`, `require_approval`, `deny`, or `error` when a rule failed to evaluate, which denies |
| `themis_gitops_refusals_total` | | Actions denied on a target a GitOps controller manages |
| `themis_approvals_total` | `result` | `approved`, `denied`, `expired`, and refused attempts: `digest_mismatch` for a digest other than the one shown, `replayed` for an action already decided |
| `themis_actions_total` (executor) | `capability`, `result` | Actions executed: `succeeded`, `stale`, `dry_run_failed`, `failed`, `ambiguous` |
| `themis_verification_outcomes_total` | `outcome` | `resolved`, `contained`, `unchanged`, `worsened`, `inconclusive` (which includes a target that could not be observed) |
| `themis_notifications_total` | `result` | Webhook posts `delivered`, or `failed` and retried |
| `themis_evidence_collected_total` | `template` | Evidence items collected |
| `themis_evidence_truncated_total` | `template` | Items cut to their template's bound |
| `themis_redactions_total` | `template` | Secrets replaced in collected evidence |
| `themis_intake_requests_total` | `source`, `code` | Intake HTTP requests, by response status |
| `themis_intake_alerts_received_total` | `source`, `result` | Alerts stored at intake, `accepted` or `invalid` |
| `themis_intake_truncated_alerts_total` | `source` | Alerts a source dropped before sending them, for example past Alertmanager's `max_alerts` |
| `themis_intake_alerts_processed_total` | `outcome`, `reason` | Alerts that `created` or `joined` an incident, were `resolved`, `stale` or `retried`, or were `rejected` for a `reason` |
| `themis_intake_refused_total` | `source`, `reason` | Intake requests refused before their alerts were read: `no_admin_account` while no admin account exists |
| `themis_intake_reads_total` | `source`, `result` | Reads of Alertmanager's API: `ok`, or why one failed: `unreachable`, `refused` (credentials), `unexpected` (not Alertmanager's answer) |
| `themis_intake_alerts_taken_total` | `source`, `status` | Alerts taken from what Themis read: `firing` when one starts, and again every 4 hours while it fires; `resolved` when it ends |

### What waits now

These gauges are read from the database at each scrape, with one query and a five-second timeout. Every core replica reports the same value, so aggregate them with `max`, not `sum`.

| Metric | Labels | Value |
|---|---|---|
| `themis_incidents` | `state` | Incidents in each state |
| `themis_approvals_pending` | | Actions awaiting approval |
| `themis_oldest_pending_approval_age_seconds` | | Age of the oldest one, or 0 when none waits |
| `themis_metrics_collection_errors_total` (counter) | | Scrapes whose query failed; those report none of the three above |

### The investigator

The investigator reports its use of the model by `route`: the configured route, or `other`.

| Metric | Labels | Counts |
|---|---|---|
| `themis_model_requests_total` | `route`, `outcome` | Model requests: `ok`, `validation_failed`, `truncated` (cut off at the output limit), `refused` (the model declined to answer), `budget_exceeded`, `ineligible`, `deadline_exceeded`, `unavailable` |
| `themis_model_request_duration_seconds` (histogram) | `route` | Time to a request's result, repairs included |
| `themis_model_tokens_total` | `route`, `kind` | Tokens the model reported, `input` and `output` |
| `themis_model_cost_usd_total` | `route` | Cost at the route's price (`model.price` in the chart). Without a price nothing is reported: a guessed price would be wrong |
| `themis_model_validation_failures_total` | `route` | Replies that did not match the schema, repaired or not |
| `themis_investigation_steps` (histogram) | | Model steps an investigation used before concluding |
| `themis_budget_exhausted_total` | `kind` | Investigations out of `tokens`, `steps`, `evidence` or `deadline` |
| `themis_core_requests_total` | `method`, `code` | Calls to the core, by gRPC method and status code |

### Queries to start from

```promql
# Actions waiting for a human, and the oldest waiting more than 15 minutes.
max(themis_approvals_pending)
max(themis_oldest_pending_approval_age_seconds) > 900

# A model proposing actions on the wrong target, or citing evidence it never saw.
sum by (kind) (increase(themis_safety_events_total[1h])) > 0

# Incidents handed to humans, by the state they left.
sum by (from) (increase(themis_escalations_total[1d]))

# How long approvals take, 90th percentile.
histogram_quantile(0.9, sum by (le) (rate(themis_stage_duration_seconds_bucket{stage="approval_wait"}[1d])))

# Investigations that couldn't decide for lack of evidence Themis could have had.
sum(increase(themis_abstentions_total{kind="blind"}[1d]))
sum by (template, cause) (increase(themis_missing_evidence_total{cause!="absent"}[1d]))

# The gauges stopped: their database query fails.
increase(themis_metrics_collection_errors_total[10m]) > 0
```

## Traces

Set an OTLP endpoint and every component exports traces to it:

```yaml
telemetry:
  otlp:
    endpoint: http://otel-collector.observability:4317
    protocol: grpc          # or http/protobuf
    egressCIDR: 10.0.0.0/8  # where the investigator may reach the collector
```

The standard `OTEL_*` variables configure the rest, sampling included. Without an endpoint nothing is traced: no exporter runs and no connection is made.

A trace is one unit of work, not one incident: processing an alert, an investigation, an approval, executing an action, a verification check. Incidents last minutes to hours across restarts, longer than traces survive in most backends. Every span carries the IDs a log line would (`themis.incident.id`, `themis.lease.id`, `themis.action.id`, `themis.proposal.id` and `themis.job.kind`), so search for `themis.incident.id` to find all of one incident's work.

- A background job runs in a trace of its own, linked to the span that queued it, since it may run long after. The link travels in the job's metadata; its arguments never carry trace context.
- A call from the investigator to the core, or from the core to the executor, continues the caller's trace. The investigator's idle polls for a lease are not traced.
- An investigation is one trace in the investigator, with a span per model request carrying the route, outcome, model and token counts, never the prompt or the reply.
- The core's HTTP API and intake are traced, with spans named by route.
- Each transition in an incident's audit trail records the trace that made it, so `GET /v1/incidents/{id}` leads from a state change to its trace.
