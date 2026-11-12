# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the DBOS executor ID of koinon-api is its pod name (ADR 0085, old issue 413).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# The ID comes from the downward API: unique per replica, never reused. With api.durable off there
# is no DBOS runtime and no executor ID.
exec_env() { render --show-only templates/deployments.yaml "$@" | grep -A4 -E -- '- name: KOINON_EXECUTOR_ID$'; }
exec_env --set api.durable.enabled=true | hit -F 'fieldPath: metadata.name' || fail "KOINON_EXECUTOR_ID is the pod name with api.durable.enabled"
if exec_env --set api.durable.enabled=false | hit  .; then fail "no KOINON_EXECUTOR_ID with api.durable.enabled=false"; fi
