# Evaluation

The evaluation cases live with their [scenario packs](../docs/scenarios.md), in `scenarios/<pack>/eval/<case>/`: each a cluster with a known fault, and the right answer. An investigation is replayed against it with no cluster, so any model can be compared on the same evidence. This folder holds the model replies saved for CI, in `recordings/`, and the reports, in `out/`.

| File in a case | What it is |
|---|---|
| `case.yaml` | What's being tested, and the right answer (`truth`) |
| `cluster.yaml` | The cluster as the controllers and the kubelet left it. Owners are linked by kind and name, and `creationTimestamp` is set by hand, since checks depend on it |
| `logs/<pod>.txt` | What each pod logged, if anything |
| `bundle.json` | Generated: what Themis's real evidence templates, redaction and the pack's checks make of the cluster |

Run `make eval-bundles` after changing a case, or code that changes what evidence looks like, and review the diff. `make scenario-check` and `go test ./eval` fail if a bundle is stale, or if a check disagrees with `truth.predicates`.

Change `version` whenever you change `truth`, so results are never compared across different answers.

## Running it

```sh
OLLAMA_CONTEXT_LENGTH=16384 ollama serve   # the default context can cut off long prompts
make eval-local                            # reports land in eval/out/local
```

Each case runs through the real prompt, loop and model client, with the bundle standing in for the cluster. The scorer is plain code: no model judges anything.

For a hosted model, run `python -m themis_intelligence.evaluation` with `--base-url`, `--model`, `--price-input` and `--price-output`, and the key in a file named by `MODEL_API_KEY_FILE`. A run stops before a call that would pass `--max-usd` (5 by default) or `--max-tokens`, going by an estimate of the call's size, so also set a limit with your provider.

A report that stopped early or lost runs is marked partial, and can't pass the safety gate. It can still fail it: harm found in part of a run is still harm.

The replay accepts the first proposal and scores it as the model made it, so naming the wrong workload counts as harmful even though the real core would refuse it. It also ignores evidence params, so asking for another pod's logs returns the default pod's.

## In CI

Every PR replays `eval/recordings/qwen3-8b`, replies saved from a real run, through the real prompt, loop and scorer, with no model involved. CI fails when:

- the replay does harm Themis would let through, or a check disagrees with a case's answer. Harm the pack's policy denies, or a proposal naming another workload, which the core refuses, is reported as harm the model attempted, but doesn't fail CI: models never authorize anything;
- the prompt, the schema or the evidence changed, so the saved replies no longer match.

After a change like that, record new replies (`make eval-record`, with `EVAL_BASE_URL` and `EVAL_MODEL` pointing at the model), read the new report in `eval/out/record`, and commit the recordings with the change. A prompt change can't land without new numbers. `make eval-record NAME=<pack>` records only that pack's cases and keeps the rest, as a new pack needs.

Everything here is public, so use only synthetic or fully redacted data.
