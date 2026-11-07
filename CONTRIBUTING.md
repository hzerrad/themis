# Contributing to Themis

Themis is in alpha, which makes this a good time to get involved. The design is still open, and a report or a pull request now can change where the project goes. Expect its structure and configuration to move between releases while that happens.

## Where to start

You don't need to write code to help.

- **Run the alpha and tell us how it went.** The [Alpha feedback](https://github.com/hzerrad/themis/issues/new?template=alpha-feedback.yml) form asks what Themis decided and whether that was right. A wrong call, with the evidence that led to it, is one of the most useful things you can send.
- **Teach Themis a new kind of incident.** Incident types are scenario packs. `make scenario NAME=my-scenario` creates one to fill in, and [docs/scenarios.md](docs/scenarios.md) walks through the rest.
- **Pick up an issue** labelled [`good first issue`](https://github.com/hzerrad/themis/labels/good%20first%20issue) or [`help wanted`](https://github.com/hzerrad/themis/labels/help%20wanted).
- **Planning something large?** Start a thread in [Discussions](https://github.com/hzerrad/themis/discussions) first, so we can agree on the approach before you spend a weekend on it.

Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md).

## Ground rules

Themis exists to keep bad changes away from clusters, so a few rules here are stricter than in most projects.

- **Changes to governance, approval, execution or verification must include tests that demonstrate the safety property they affect.** A bug there may not crash anything. It can let through a change that should have been stopped.
- **Model output is untrusted input everywhere**, including inside `themis-intelligence`. A model can be wrong, and logs or events can carry text written to steer it.
- **A new scenario pack comes with evaluation cases**, as [docs/scenarios.md](docs/scenarios.md) describes. They show how the investigator handles the new incident, and whether a harmful fix could get through.
- **Text people read follows [docs/writing.md](docs/writing.md):** the web UI, the messages Themis writes, and the docs for operators. Themis has one voice, plain and precise, and the guide describes it.
- **Keep each pull request to one logical change.** Small ones get reviewed faster.

## Commit messages and pull requests

We squash-merge, so your pull request's title becomes the commit on `main`. Write it as one plain sentence that says what changes and why it matters, without a type prefix like `feat:`:

```
Roll back only the changes of a rollout that broke it
```

Keep each commit to a one-line subject and a sign-off, and put the details in the pull request's description.

Scenario packs and bug fixes have their own templates. Add `?expand=1&template=scenario-pack.md` or `?expand=1&template=bug-fix.md` to the compare URL to use one.

## Developer Certificate of Origin

Every commit needs a sign-off. It certifies the [Developer Certificate of Origin](https://developercertificate.org/): that you wrote the change, or have the right to submit it under the project's license.

```bash
git commit -s
```

CI checks that each commit in a pull request carries a `Signed-off-by` matching its author.

## Running the checks locally

You need Docker, plus Go, [uv](https://docs.astral.sh/uv/), Node, Helm and kubectl at the versions pinned in `mise.toml`. With [mise](https://mise.jdx.dev/), `mise install` sets them all up. The other tools (buf, golangci-lint, kind, envtest) are pinned in `tools/go.mod` and run through `go tool`, so there's nothing more to install.

```bash
make help            # list targets
make lint            # lint Go, Python, protobuf
make test            # unit and integration tests (PostgreSQL runs in Docker), and the web UI's
make e2e             # end-to-end scenarios on a fresh kind cluster, with a fake model
make scenario-check  # every scenario pack's cases, replayed against recorded model replies
make vuln            # known vulnerabilities in the Go and Python dependencies
```

Run `make lint test` before you open a pull request. CI runs the same targets, plus `make e2e`, which plays the bad-rollout scenarios on a fresh kind cluster with a fake model. It needs only Docker, so you can run it locally too.

`make test` includes `make test-ui`. It loads the web UI in jsdom against what the API serves for incidents in every state, built by the Go views, visits every page as an admin, an approver and a viewer, and works the forms. It fails on an uncaught exception or rejection, a `console.error`, a "Not loaded:" box, or text left by a missing value, such as `null` or `NaN`. If you add a page or a state, add it to the fixtures (`internal/server/httpapi/uifixtures_test.go`) and the visits (`test/ui/smoke.mjs`).

Changing the investigation prompt or loop? Judge it against a real model. [Running locally](docs/demo.md#running-locally) shows how.

## Security issues

Please don't report vulnerabilities in public issues. [SECURITY.md](SECURITY.md) explains how to report them privately.
