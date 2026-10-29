#!/usr/bin/env bash
# Prints a KUBEBUILDER_ASSETS directory for the next Kubernetes release, so the
# tests can run against it before it ships. envtest publishes only released
# versions, so the API server is the newest one Kubernetes has published
# (alpha, beta or release candidate), checked against its checksum; etcd and
# kubectl come from the envtest release in BASE.
#
#   scripts/envtest-next.sh BASE      BASE: envtest binaries of the newest supported minor
#
# Linux only: Kubernetes publishes kube-apiserver for Linux alone.
set -euo pipefail

BASE=$1
ROOT=$(cd "$(dirname "$0")/.." && pwd)
VERSION=${NEXT_K8S_VERSION:-$(curl -fsSL https://dl.k8s.io/release/latest.txt)}
ARCH=$(go env GOARCH)
DIR="$ROOT/bin/envtest/next/$VERSION-linux-$ARCH"

if [ "$(go env GOOS)" != linux ]; then
  echo "envtest-next: kube-apiserver is published for Linux only" >&2
  exit 1
fi

if [ ! -x "$DIR/kube-apiserver" ]; then
  mkdir -p "$DIR"
  url="https://dl.k8s.io/release/$VERSION/bin/linux/$ARCH/kube-apiserver"
  curl -fsSL -o "$DIR/kube-apiserver.tmp" "$url"
  echo "$(curl -fsSL "$url.sha256")  $DIR/kube-apiserver.tmp" | sha256sum --check --quiet >&2
  chmod +x "$DIR/kube-apiserver.tmp"
  cp "$BASE/etcd" "$BASE/kubectl" "$DIR/"
  mv "$DIR/kube-apiserver.tmp" "$DIR/kube-apiserver"
fi

echo "envtest-next: kube-apiserver $VERSION" >&2
echo "$DIR"
