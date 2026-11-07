#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Keep a port-forward to the OpenShell gateway alive across gateway restarts.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail
PORT=${PORT:-18080}
while true; do
  kubectl --context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}" -n openshell port-forward svc/openshell "${PORT}:8080"
  sleep 2
done
