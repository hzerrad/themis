# Contributing to Themis

Thank you for your interest in Themis. The project is in early development, so expect the structure to change.

## Where to start

- **Try an alpha** and tell us what you found with the **Alpha feedback** issue form.
- **Add an incident type.** Scenario packs are how Themis learns to handle new kinds of incident. Start with [docs/scenarios.md](docs/scenarios.md) and `make scenario NAME=my-scenario`.
- **Pick an issue** labelled `good first issue` or `help wanted`.

Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md).

## Ground rules

- Changes that touch governance, approval, execution, or verification must include tests that demonstrate the safety property they affect.
- Model output is untrusted input everywhere, including inside the intelligence component.
- Keep pull requests small and focused on one logical change.

## Commit messages and pull requests

Pull requests are squash-merged, so the pull request's title becomes the commit on `main`. Write it as one plain sentence that says what changes and why it matters, without a type prefix:

```
Raise or roll back by whether a healthy earlier version exists
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

Run `make lint test` before opening a pull request. CI runs the same targets, and `make e2e`, which runs the bad-rollout scenarios on a fresh kind cluster with a fake model, so it needs only Docker.

`make test` includes `make test-ui`, which runs the web UI: it loads the page in jsdom against what the API serves, built by the Go views from incidents in every state, visits every page as an admin, an approver and a viewer, and works the forms. It fails on an uncaught exception or rejection, a `console.error`, a "Not loaded:" box, and text a missing value leaves, such as `null` or `NaN`. A new page or state belongs in its fixtures (`internal/server/httpapi/uifixtures_test.go`) and its visits (`test/ui/smoke.mjs`).

Changes to the investigation prompt or loop are easiest to judge against a real model: see [Running locally](docs/demo.md#running-locally).

## Reporting security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).
