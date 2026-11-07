#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Spike step 1: install Kubernetes Agent Sandbox (core plus extensions) from the
# pinned release manifest. Upstream ships no Helm chart for it.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

VERSION=v1.0.7
SHA256=136c10c9347cb5a6740b351444cae82f404e883c3f520272257ba785f2af0da8
URL="https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${VERSION}/sandbox-with-extensions.yaml"
KUBECTL=(kubectl --context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}")

tmp=$(mktemp -d "${TMPDIR:-/tmp}/agent-sandbox.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/manifest.yaml" "$URL"
echo "${SHA256}  $tmp/manifest.yaml" | shasum -a 256 -c -

"${KUBECTL[@]}" apply --server-side -f "$tmp/manifest.yaml"
"${KUBECTL[@]}" -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=180s
