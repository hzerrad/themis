# Releasing

Themis has one version for all of it: the three images and the Helm chart. The core, the executor and the investigator talk over a contract that changes between releases, so they only work as a set.

## What gets published

| When | Images | Chart |
|---|---|---|
| A merge to `main` | `themis-{core,executor,intelligence}:main` and `:sha-<commit>`: the edge build | Not published; the chart in the repository uses `main` |
| A version tag, `v0.2.0` | The same three images as `:v0.2.0` (and `:sha-<commit>`), signed | `oci://ghcr.io/hzerrad/charts/themis` version `0.2.0`, pinned to the `v0.2.0` images, plus a GitHub release with notes from the merged pull requests |

Each image tag is one index for linux/amd64 and linux/arm64, and the signature is on the index.

Versions follow [semantic versioning](https://semver.org). Below 1.0, a minor version (`0.2.0` to `0.3.0`) may change behaviour or configuration; the release notes say how.

## Cutting a release

From an up-to-date `main` whose checks, including the end-to-end run, are green, and that names the new version where the docs install it (the README, `docs/quickstart.md` and the chart README):

```sh
git tag -s v0.2.0 -m "Themis v0.2.0"
git push origin v0.2.0
```

The Images workflow builds and signs the images, then publishes the chart and the GitHub release. Edit the release's notes on GitHub afterwards if they need an upgrade note.

### Alphas and other pre-releases

A tag with a pre-release part, such as `v0.1.0-alpha1`, publishes the same way: images `:v0.1.0-alpha1`, chart version `0.1.0-alpha1`. Its GitHub release is marked as a pre-release and never becomes the latest release. Helm installs a pre-release only when asked for it by version, so the install line names it:

```sh
helm install themis oci://ghcr.io/hzerrad/charts/themis --version 0.1.0-alpha1 \
  --namespace themis --create-namespace \
  --set enrolledNamespaces='{shop}'
```

## Installing a release

Each namespace in `enrolledNamespaces` must exist before the install.

```sh
kubectl create namespace shop
helm install themis oci://ghcr.io/hzerrad/charts/themis --version 0.2.0 \
  --namespace themis --create-namespace \
  --set enrolledNamespaces='{shop}'
```

To check an image's signature:

```sh
cosign verify ghcr.io/hzerrad/themis-core:v0.2.0 \
  --certificate-identity-regexp 'https://github.com/hzerrad/themis/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
