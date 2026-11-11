# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: a held copy must not act with its source's outward settings (ADR 0110, issue #516):
# checkpoints.readOnly (restore-only checkpoints) and the NOTES warnings for restoreHold.
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

cp_args=(--set checkpoints.enabled=true --set checkpoints.s3.existingSecret=cp --set checkpoints.s3.bucket=b)

# Off by default, and without checkpoints the value renders nothing.
lacks "no read-only checkpoints by default" 'KOINON_CHECKPOINTS_READ_ONLY' "${cp_args[@]}"
lacks "readOnly without checkpoints renders nothing" 'KOINON_CHECKPOINTS_READ_ONLY' --set checkpoints.readOnly=true
env_is KOINON_CHECKPOINTS_READ_ONLY '"true"' "${cp_args[@]}" --set checkpoints.readOnly=true
# The value is a boolean: a typo fails the render instead of silently leaving the copy writable.
if render "${cp_args[@]}" --set checkpoints.readOnly=yes-please >/dev/null 2>&1; then fail "checkpoints.readOnly must be a boolean"; fi

# NOTES are printed by install, not by template.
held_notes() {
	local out rc=0
	out=$("$HELM" install koinon "$CHART" -n koinon --dry-run=client --set openshell.insecure=true --set "$ADDRESS" "$@" 2>&1) || rc=$?
	if [[ $rc -ne 0 ]]; then
		fail "helm install --dry-run=client for the restoreHold NOTES failed: $(head -1 <<<"$out")"
		return 0
	fi
	printf '%s\n' "$out"
}
n_plain=$(held_notes)
if grep -q 'restoreHold is on' <<<"$n_plain"; then fail "NOTES warn about restoreHold on an install that is not held"; fi
n_held=$(held_notes --set restoreHold=true)
grep -q 'restoreHold is on' <<<"$n_held" || fail "NOTES do not warn about a held copy's outward settings"
grep -q 'the outward switch' <<<"$n_held" || fail "NOTES do not mention the outward switch on a held install"
if grep -q 'checkpoints.readOnly is false' <<<"$n_held"; then fail "NOTES warn about checkpoints on a held install that has none"; fi
n_cp=$(held_notes --set restoreHold=true "${cp_args[@]}")
grep -q 'checkpoints.readOnly is false' <<<"$n_cp" || fail "NOTES do not warn that a held install with checkpoints writes into its bucket"
n_ro=$(held_notes --set restoreHold=true "${cp_args[@]}" --set checkpoints.readOnly=true)
if grep -q 'checkpoints.readOnly is false' <<<"$n_ro"; then fail "NOTES warn about checkpoints although readOnly is true"; fi
