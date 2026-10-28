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

## Local checks

Run `make lint test` before opening a pull request. CI runs the same targets, and `make e2e`, which runs the bad-rollout scenarios on a fresh kind cluster with a fake model, so it needs only Docker.

Changes to the investigation prompt or loop are easiest to judge against a real model: see [Running locally](docs/demo.md#running-locally).

## Reporting security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).
