# Security policy

Themis can change workloads in the clusters it runs in. A flaw that lets a change get past its controls is a security vulnerability.

## Reporting a vulnerability

Report it privately through [GitHub's private vulnerability reporting](https://github.com/hzerrad/themis/security/advisories/new). Do not open a public issue, discussion or pull request.

Include:

- the affected component (`themis-core`, `themis-intelligence`, `themis-executor` or the Helm chart) and its version;
- the steps to reproduce, and the configuration they need;
- the impact you observed.

You receive an acknowledgement within 7 days. Once a fix is released, the report is published as a GitHub security advisory, crediting you unless you ask otherwise.

## Scope

In scope, for example:

- A change applied, or put to a person for approval, without the controls described under [Governance](README.md#governance): a valid target, a justifying check, policy, and an approval bound to the change.
- Text in logs, events or other evidence that makes Themis act on another workload, or gets a harmful fix past its controls.
- Secrets reaching the model, the web UI, the logs or the database without redaction.
- Bypassing authentication or roles in the web UI, the HTTP API or the gRPC API.
- A component using Kubernetes permissions beyond those [documented](docs/architecture.md#what-protects-what).
- The executor applying a fix outside `executor.namespaces` or `executor.fixes`.

Out of scope:

- A wrong diagnosis or fix from the investigator that Themis's controls refuse. Report it as a [bug](https://github.com/hzerrad/themis/issues/new?template=bug.yml); it may become an evaluation case.
- Attacks that already need cluster-admin rights, or write access to Themis's namespace, Secrets or database.
- A change applied through an approval written straight into Themis's database, within `executor.namespaces`, `executor.fixes` and the checks against the live cluster. The executor trusts the approvals recorded there.
- Reading or changing traffic between Themis's components from inside the cluster network. gRPC there is not encrypted yet: the NetworkPolicy limits who can reach it, and mTLS is planned.
- The investigator reaching an address within `investigator.egressCIDR` on the model's port. Set the CIDR to the model's addresses to narrow it.
- Vulnerabilities in dependencies with no impact specific to Themis. Report them upstream.

## Supported versions

Themis is in alpha. Security fixes land on `main` and ship in the next pre-release. Earlier pre-releases are not patched: upgrade to the newest one.

## Verifying releases

The release workflow signs every image with cosign. To check one:

```sh
cosign verify ghcr.io/hzerrad/themis-core:v0.1.0-alpha1 \
  --certificate-identity-regexp 'https://github.com/hzerrad/themis/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
