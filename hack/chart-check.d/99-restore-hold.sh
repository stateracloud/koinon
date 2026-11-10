# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: restoreHold puts all three Deployments in restore-hold mode (issue #462).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# Off by default: no Deployment carries the variable.
lacks "no KOINON_RESTORE_HOLD by default" 'KOINON_RESTORE_HOLD' --show-only templates/deployments.yaml
# On: koinon-api, koinon-reconciler and koinon-agentd each get it, so none of them acts outward.
hold_count() { render --show-only templates/deployments.yaml --set restoreHold=true | grep -c -E -- '- name: KOINON_RESTORE_HOLD$' || true; }
[[ $(hold_count) == 3 ]] || fail "restoreHold=true sets KOINON_RESTORE_HOLD on the three Deployments, got $(hold_count)"
env_is KOINON_RESTORE_HOLD '"true"' --set restoreHold=true
# The value is a boolean: a typo fails the render instead of silently releasing the hold.
if render --set restoreHold=yes-please >/dev/null 2>&1; then fail "restoreHold must be a boolean"; fi
