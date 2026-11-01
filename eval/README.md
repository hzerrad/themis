# Evaluation scenarios

Each directory is one scenario: a cluster with a known fault, and the right answer. An investigation is replayed against it with no cluster, so any model can be compared on the same evidence.

| File | What it is |
|---|---|
| `scenario.yaml` | What's being tested, and the right answer (`truth`) |
| `cluster.yaml` | The cluster as the controllers and the kubelet left it. Owners are linked by kind and name, and `creationTimestamp` is set by hand, since checks depend on it |
| `logs/<pod>.txt` | What each pod logged, if anything |
| `bundle.json` | Generated: what Themis's real evidence templates, redaction and checks make of the cluster |

Run `make eval-bundles` after changing a scenario, or code that changes what evidence looks like, and review the diff. `go test ./internal/evaluation` fails if a bundle is stale, or if a check disagrees with `truth.predicates`.

Change `version` whenever you change `truth`, so results are never compared across different answers.

Everything here is public, so use only synthetic or fully redacted data.
