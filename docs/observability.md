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
