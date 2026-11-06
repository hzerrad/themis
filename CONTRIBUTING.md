# Contributing to Themis

Thank you for your interest in Themis. The project is in early development, so expect the structure to change.

## Ground rules

- Changes that touch governance, approval, execution, or verification must include tests that demonstrate the safety property they affect.
- Model output is untrusted input everywhere, including inside the intelligence component.
- Keep pull requests small and focused on one logical change.

## Commit messages

Themis uses [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<optional scope>): <imperative summary>
```

Common types: `feat`, `fix`, `test`, `docs`, `refactor`, `build`, `ci`, `chore`. Scopes follow the component, for example `core`, `executor`, `intelligence`, `api`, `deploy`.

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
