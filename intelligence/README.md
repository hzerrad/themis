# themis-intelligence

The investigator: a bounded investigation loop and governed model access. It holds no Kubernetes or database credentials. It leases an investigation from `themis-core` over gRPC, asks for evidence by template name, submits proposals, and reports a conclusion. The core decides what any of it may do. See the [project README](../README.md).

## Run

```bash
uv sync
THEMIS_CORE_ADDR=127.0.0.1:9090 THEMIS_TOKEN_FILE=../bin/dev/investigator-token \
MODEL_BASE_URL=http://127.0.0.1:11434/v1 MODEL_NAME=qwen3:8b \
uv run python -m themis_intelligence
```

| Variable | Default | Meaning |
|---|---|---|
| `THEMIS_CORE_ADDR` | `localhost:9090` | The core's gRPC address |
| `THEMIS_TOKEN_FILE` | `/var/run/secrets/themis/investigator-token` | The investigator's bearer token |
| `MODEL_BASE_URL` | `http://localhost:11434/v1` | An OpenAI-compatible chat completions API |
| `MODEL_NAME` | `qwen3:8b` | The model |
| `MODEL_API_KEY_FILE` | none | A file holding the endpoint's API key |
| `MODEL_CEILING` | `sensitive` | The most sensitive evidence the endpoint may receive |
| `MODEL_TOKEN_BUDGET` | `200000` | Tokens per investigation |

## How an investigation runs

- Each step, the model answers with one typed decision: read another evidence template, propose a fix, or conclude that the evidence does not support one. Replies are validated against a JSON schema, and an invalid reply is repaired once.
- Evidence reaches the model fenced as untrusted data. A template outside the profile's allowed set is refused without asking the core.
- A proposal's target is forwarded exactly as the model wrote it: the core decides, and records a wrong target as a safety event.
- A proposal waits until the failing pod's logs have been read.
- Every model call reserves its tokens from the investigation's budget first, and the investigation stops at its step limit or deadline.
- A heartbeat renews the lease. When the core says the lease is lost, for example because the incident closed, the investigation stops.

## Develop

```bash
uv run pytest
uv run ruff check && uv run ruff format --check && uv run pyright
```

The generated gRPC code in `gen/` comes from `make gen` at the repository root.
