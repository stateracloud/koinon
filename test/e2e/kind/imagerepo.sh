#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints the image repository one kind cluster uses on the shared local registry (#45):
# <registry>/<cluster>, e.g. localhost:5001/koinon-e1. All kind clusters on one Docker host share the
# registry (registry.sh), so each cluster gets its own path; two runs (two lanes, or a human's
# koinon-e1 next to e2e-remote's runner cluster) then never overwrite each other's :e2e tags.
# images.sh pushes there; run.sh and the drill rewrite the static `localhost:5001` in the values files,
# fakes.yaml and the topology to it.
#
#   KIND_REGISTRY=localhost:5001  CLUSTER=koinon-e1

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
echo "${KIND_REGISTRY:-localhost:5001}/${CLUSTER:-koinon-e1}"
