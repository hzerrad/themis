# Contributing to Themis

Themis is in alpha, and it grows with the people who contribute to it. Expect its structure and configuration to change between releases. This guide covers where to start, the rules for changes, and the checks to run before a pull request.

## Where to start

- **Try the alpha** and tell us what Themis decided, and whether it was right, with the [Alpha feedback](https://github.com/hzerrad/themis/issues/new?template=alpha-feedback.yml) form.
- **Add an incident type.** Scenario packs are how Themis learns to handle new kinds of incident. Start with [docs/scenarios.md](docs/scenarios.md) and `make scenario NAME=my-scenario`.
- **Pick an issue** labelled [`good first issue`](https://github.com/hzerrad/themis/labels/good%20first%20issue) or [`help wanted`](https://github.com/hzerrad/themis/labels/help%20wanted).
- **Ask** in [Discussions](https://github.com/hzerrad/themis/discussions) before starting something large.

Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md).

## Ground rules

- Changes that touch governance, approval, execution, or verification must include tests that demonstrate the safety property they affect.
- Model output is untrusted input everywhere, including inside the intelligence component.
- A new scenario pack comes with evaluation cases, as [docs/scenarios.md](docs/scenarios.md) describes.
- Text people read, in the web UI, in the messages Themis writes and in operator docs, follows [docs/writing.md](docs/writing.md).
- Keep pull requests small and focused on one logical change.

## Commit messages and pull requests

Pull requests are squash-merged, so the pull request's title becomes the commit on `main`. Write it as one plain sentence that says what changes and why it matters, without a type prefix:

```
Roll back only the changes of a rollout that broke it
```

Keep each commit to a one-line subject and a sign-off. Explain the details in the pull request's description.

There are templates for common kinds of change. Add `?expand=1&template=scenario-pack.md` or `?expand=1&template=bug-fix.md` to the compare URL to use one.

## Developer Certificate of Origin

All commits must be signed off to certify the [Developer Certificate of Origin](https://developercertificate.org/):

```bash
git commit -s
```

CI checks that every commit of a pull request carries a `Signed-off-by` matching its author.

## Local checks

You need Docker, plus Go, [uv](https://docs.astral.sh/uv/), Node, Helm and kubectl at the versions pinned in `mise.toml`: with [mise](https://mise.jdx.dev/), `mise install` installs them. The other tools (buf, golangci-lint, kind, envtest) are pinned in `tools/go.mod` and run through `go tool`.

```bash
make help            # list targets
make lint            # lint Go, Python, protobuf
make test            # unit and integration tests (PostgreSQL runs in Docker), and the web UI's
make e2e             # end-to-end scenarios on a fresh kind cluster, with a fake model
make scenario-check  # every scenario pack's cases, replayed against recorded model replies
make vuln            # known vulnerabilities in the Go and Python dependencies
```

Run `make lint test` before opening a pull request. CI runs the same targets, and `make e2e`, which runs the bad-rollout scenarios on a fresh kind cluster with a fake model, so it needs only Docker.

`make test` includes `make test-ui`, which runs the web UI: it loads the page in jsdom against what the API serves, built by the Go views from incidents in every state, visits every page as an admin, an approver and a viewer, and works the forms. It fails on an uncaught exception or rejection, a `console.error`, a "Not loaded:" box, and text a missing value leaves, such as `null` or `NaN`. A new page or state belongs in its fixtures (`internal/server/httpapi/uifixtures_test.go`) and its visits (`test/ui/smoke.mjs`).

Changes to the investigation prompt or loop are easiest to judge against a real model: see [Running locally](docs/demo.md#running-locally).

## Reporting security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).
