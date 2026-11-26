#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints a kind e2e values file with its placeholders filled in (used by test/e2e/run.sh and its test):
#
#   values-rewrite.sh <values file> <image repository> [<kind network CIDR>]
#
# - koinon-values.yaml (OpenShell): every localhost:5001 becomes the image repository of the cluster.
# - substrate-values.yaml: only the `registry: localhost:5001` line of the image settings changes. The cache upstream host
#   `localhost:5001` must stay as it is: it is the registry host of the harness image reference, and the mirror key
#   of the image cache (a different key leaves the reference unrewritten and atelet dials the registry itself).
#   The kind network CIDR (docker network inspect kind) replaces the 172.18.0.0/16 of the cache egress rule.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

(($# >= 2)) || { echo "usage: $0 <values file> <image repository> [<kind network CIDR>]" >&2; exit 2; }
file=$1
repo=$2
cidr=${3:-}

if [[ $(basename "$file") == substrate-values.yaml ]]; then
  out=$(sed "s#^\\( *registry: \\)localhost:5001\$#\\1$repo#" "$file")
  if [[ -n $cidr ]]; then
    out=${out//172.18.0.0\/16/$cidr}
  fi
  printf '%s\n' "$out"
else
  sed "s#localhost:5001#$repo#g" "$file"
fi
