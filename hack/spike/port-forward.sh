#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Keep a port-forward to the OpenShell gateway alive across gateway restarts.
set -uo pipefail
PORT=${PORT:-18080}
while true; do
  kubectl --context homelab -n openshell port-forward svc/openshell "${PORT}:8080"
  sleep 2
done
