#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints the image repository of an existing kind cluster on the shared local registry (#45, #601):
# <registry>/<cluster>, the registry host read from the cluster's KEP-1755 ConfigMap
# (kube-public/local-registry-hosting, written by registry.sh attach). imagerepo.sh gives the same path
# from the environment before a cluster exists; scripts that run against a cluster that is already up
# (test/escape/kind.sh, hack/rt1-fixtures-kind.sh) use this one, so they push where the node pulls.
# Exits 1 when the cluster has no such ConfigMap: run `make dev-kind-up`.
#
#   clusterrepo.sh <cluster>      (the kube context is kind-<cluster>)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cluster=${1:?usage: clusterrepo.sh <cluster>}
here="$(cd "$(dirname "$0")" && pwd)"
host=$(kubectl --context "kind-$cluster" -n kube-public get configmap local-registry-hosting \
  -o jsonpath='{.data.localRegistryHosting\.v1}' 2>/dev/null | sed -n 's/^host: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' || true)
[[ -n $host ]] || { echo "kind cluster $cluster has no local registry (kube-public/local-registry-hosting): run 'make dev-kind-up'" >&2; exit 1; }
KIND_REGISTRY="$host" CLUSTER="$cluster" "$here/imagerepo.sh"
