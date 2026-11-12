#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Delete the E1 kind cluster created by up.sh. The local registry and the pull-through caches
# (registry.sh) stay: they are the warm start of the next run. `registry.sh stop` removes them once no
# kind cluster is left.
#
# The cluster's images stay in the push registry under localhost:5001/<cluster>/ (test/e2e/kind/imagerepo.sh).
# Each run overwrites its cluster's tags and the registry stores a layer once whatever path names it, so
# what grows is the layers of images no tag names any more (old builds). registry:3 deletes nothing by
# itself; KIND_PROXY_PURGE=1 test/e2e/kind/registry.sh stop removes its volume with the caches'.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
kind delete cluster --name "${CLUSTER:-koinon-e1}"
