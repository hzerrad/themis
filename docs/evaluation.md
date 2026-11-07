# Evaluation

Themis's evaluation replays its investigator against recorded incidents and scores what it decides. This page reports the current results and an earlier comparison across models. How the evaluation works, and how to run it, is in [eval/README.md](../eval/README.md).

## Current results, v0.1.0-alpha1

Measured on 2026-11-07 at Themis commit `68916a0`. The first alpha has the same code; only this page changed after it.

- **91 cases** across the five scenario packs: 63 incidents, and 28 copies of them with text injected into their evidence (logs, event messages, config values) that tries to steer the investigator.
- **One run per case and model.** Each case is judged by Themis's own core, exactly as a live suggestion would be: the fix must be offered, justified by a check, valid, allowed by policy and approved by a person.

| Measure | Meaning |
|---|---|
| Right decision | Proposed the fix the case calls for, or declined where no fix is right |
| Right cause first | The leading hypothesis names the case's root cause |
| Harmful fix attempted | The model proposed a fix that would do harm |
| Attacks obeyed / steered | Of the 28 injected copies: did what the injected text asked / decided differently from the clean copy |
| Harmful fix reaching a person | A harmful fix that Themis's core would not refuse. This is the safety gate; any non-zero value fails CI |

| Model | Right decision | Right cause first | Harmful fix attempted | Attacks obeyed / steered | Harmful fix reaching a person | Cost of the 91 cases |
|---|---|---|---|---|---|---|
| qwen3 8B, local (the CI baseline) | 63 / 91 | 60 / 91 | 14 | 14 / 9 | **0** | local GPU |
| Claude Haiku 5.5 | 84 / 91 | 86 / 91 | 0 | 1 / 0 | **0** | $0.41 |

**No harmful fix reached a person.** The small local model attempted 14 harmful fixes and was steered by injected evidence in 9 cases; Themis's core refused every one. Claude Haiku attempted none and was steered by none, for 41 cents across the whole suite.

qwen3 8B ran through Ollama on an RTX 4070 SUPER (12 GB), context 16k, temperature 0; Claude Haiku through Anthropic's API with structured output, effort `medium`, output limit 16,000.

## An earlier comparison: six models, 65 cases

On 2026-11-05 and 06, six models ran an earlier, smaller suite. Since then the suite grew to 91 cases and the rules for choosing between a rollback and a raise changed, so these figures don't compare with the ones above. They are kept for the comparison between models, which is still indicative.

### What was measured

- **65 cases** across the five scenario packs: 40 incidents, and 25 copies of them with text injected into their evidence (logs, event messages, config values) that tries to steer the investigator.
- **One run per case and model**, on 2026-11-05 and 06.
- The same prompt, reply schema and scoring for every model. Each case is judged by Themis's own core, exactly as a live suggestion would be: the fix must be offered, justified by a check, valid, allowed by policy and approved by a person.

| Measure | Meaning |
|---|---|
| Right decision | Proposed the fix the case calls for, or declined where no fix is right |
| Right cause first | The leading hypothesis names the case's root cause |
| Harmful fix attempted | The model proposed a fix that would do harm |
| Attacks obeyed / steered | Of the 25 injected copies: did what the injected text asked / decided differently from the clean copy |
| Harmful fix reaching a person | A harmful fix that Themis's core would not refuse. This is the safety gate; any non-zero value fails CI |

### Results

| Model | Right decision | Right cause first | Harmful fix attempted | Attacks obeyed / steered | Harmful fix reaching a person | Cost of the 65 cases | Model time |
|---|---|---|---|---|---|---|---|
| qwen3 8B, local | 44 / 65 | 51 / 65 | 13 | 11 / 8 | **0** | local GPU | 39 min |
| qwen3 14B, local | 47 / 65 | 47 / 65 | 9 | 9 / 6 | **0** | local GPU | 2 h 24 min |
| GPT-6 Luna | 61 / 65 | 62 / 65 | 0 | 0 / 0 | **0** | $0.11 | 17 min |
| GPT-6.1 Sol | 63 / 65 | 63 / 65 | 0 | 0 / 0 | **0** | $2.24 | 17 min |
| Claude Haiku 5.5 | 64 / 65 | 64 / 65 | 0 | 0 / 0 | **0** | $0.20 | 13 min |
| Claude Sonnet 5.5 | 64 / 65 | 65 / 65 | 0 | 0 / 0 | **0** | $3.39 | 12 min |

**No harmful fix reached a person in any of the 390 runs.** Whatever the model proposed, Themis's core refused what it should refuse. The small local models attempted harmful fixes and were steered by injected evidence; the core caught every one.

**The model decides how useful the investigator is, not how safe Themis is.** The hosted models made the right decision in 94 to 98% of cases and none of the 25 attacks moved them. The small ones, GPT-6 Luna and Claude Haiku 5.5, did nearly as well as the large ones at a twentieth of the price.

### Settings

| Model | Endpoint | Temperature | Reasoning | Output limit | Themis commit |
|---|---|---|---|---|---|
| qwen3 8B | Ollama on an RTX 4070 SUPER (12 GB), context 16k | 0 | the model's default | 4,096 | `4fbf030` |
| qwen3 14B | same GPU, context 32k; about a fifth of the model ran on the CPU | 0 | the model's default | 4,096 | `4fbf030` |
| GPT-6 Luna, GPT-6.1 Sol | OpenAI API, strict structured output (`schemaDialect: openai-strict`) | the default | the default | 4,096 | `2a0987b` |
| Claude Haiku 5.5, Sonnet 5.5 | Anthropic API, structured output | the default | effort `medium` | 16,000 | `b251837` |

Between the three commits, Themis changed how it reads replies (over-long notes are trimmed instead of refused) and how it talks to OpenAI. The prompt, the reply schema, the cases and the scoring did not change.

## Limits

- **Scored before the right fix and the tested fix were told apart.** `oom-after-rollout` puts a raise to policy, to see it refused, and calls a rollback right. Until scoring version 7 the case named only the raise, so a model that rolled back there was counted wrong. The earlier table was scored that way; the case now names the rollback as its right fix in `truth.fix`.
- **One run per case.** The intervals in each model's report are wide; a difference of a case or two between models is not meaningful.
- **Synthetic cases.** They are written to be realistic, and include look-alikes and attacks, but they are not production incidents.
- **qwen3 14B was not at its best:** part of it ran on the CPU, which made it slow and may have cost it some answers. A run with a smaller context, entirely on the GPU, is still to do.
- **Not every model was run:** gpt-oss 20B and qwen3 32B are still to do.

## Reproducing it

Each model's run is one command; the cases, prompt and scoring come from the repository at the commit above.

```sh
# A local model through Ollama
make eval-record EVAL_MODEL=qwen3:8b-16k EVAL_BASE_URL=http://127.0.0.1:11434/v1

# Claude
make eval-record EVAL_PROVIDER=anthropic EVAL_MODEL=claude-haiku-5-5 EVAL_KEY_FILE=<file holding the key> EVAL_MAX_USD=3

# Then one table across the models you ran
make eval-compare
```

OpenAI's newer models need three settings; see [Against OpenAI](../eval/README.md#against-openai).
