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

# Restore-only checkpoints reach the reconciler too: it must not wait for a backup agentd will not write
# (ADR 0110 decision 4). The value is in the config file the reconciler reads.
lacks "the reconciler is not restore-only by default" 'checkpointsReadOnly: true' "${cp_args[@]}"
has "the reconciler learns that checkpoints are restore-only" 'checkpointsReadOnly: true' "${cp_args[@]}" --set checkpoints.readOnly=true

# The release of a restored copy (ADR 0110 decision 1): the value, the Job and the NOTES warning.
lacks "no importer accept by default" 'KOINON_RESTORE_ACCEPT_IMPORTER_DELETIONS'
env_is KOINON_RESTORE_ACCEPT_IMPORTER_DELETIONS '"true"' --set restore.acceptImporterDeletions=true
if render --set restore.acceptImporterDeletions=yes-please >/dev/null 2>&1; then fail "restore.acceptImporterDeletions must be a boolean"; fi
if render --set restore.nonsense=1 >/dev/null 2>&1; then fail "an unknown restore.* value must fail the render"; fi
rc_job=$(render --show-only templates/restore-release-check.yaml)
grep -q 'helm.sh/hook: pre-upgrade' <<<"$rc_job" || fail "the restore-release-check Job is not a pre-upgrade hook"
grep -q 'helm.sh/hook-weight: "10"' <<<"$rc_job" || fail "the restore-release-check Job does not run after the migrate Job (weight 0)"
grep -q 'argocd.argoproj.io/sync-wave: "2"' <<<"$rc_job" || fail "the restore-release-check Job has no Argo CD sync wave after the migrate Job"
grep -qE 'args: \["restore", "release-check"\]' <<<"$rc_job" || fail "the restore-release-check Job does not run koinon-api restore release-check"
grep -q 'optional: true' <<<"$rc_job" || fail "the restore-release-check Job must tolerate a missing install mark Secret (first install under Argo CD)"
if grep -q 'KOINON_RESTORE_ACCEPT_IMPORTER_DELETIONS' <<<"$rc_job"; then fail "the Job sees the accept value without it being set"; fi
rc_accept=$(render --show-only templates/restore-release-check.yaml --set restore.acceptImporterDeletions=true)
grep -q 'KOINON_RESTORE_ACCEPT_IMPORTER_DELETIONS' <<<"$rc_accept" || fail "the Job does not get restore.acceptImporterDeletions"
rc_held=$(render --set restoreHold=true --show-only templates/restore-release-check.yaml 2>&1 || true)
if grep -q 'kind: Job' <<<"$rc_held"; then fail "the restore-release-check Job is rendered on a held install"; fi
n_acc=$(held_notes --set restore.acceptImporterDeletions=true)
grep -q 'restore.acceptImporterDeletions is set' <<<"$n_acc" || fail "NOTES do not warn that restore.acceptImporterDeletions is set"
if grep -q 'restore.acceptImporterDeletions is set' <<<"$n_plain"; then fail "NOTES warn about restore.acceptImporterDeletions when it is false"; fi

# The release-pending alert (ADR 0110 decision 1): koinon-api's gauge, 15 minutes.
rp=(--set metrics.prometheusRule.enabled=true --api-versions monitoring.coreos.com/v1/PrometheusRule --show-only templates/prometheusrule.yaml)
has "release-pending alert renders" 'alert: KoinonRestoreReleasePending$' "${rp[@]}"
has "release-pending alert renders without durable" 'alert: KoinonRestoreReleasePending$' "${rp[@]}" --set api.durable.enabled=false
has "release-pending alert reads the max of the gauge" 'expr: max\(koinon_restore_release_pending\) == 1$' "${rp[@]}"
has "release-pending alert waits 15m" 'for: 15m$' "${rp[@]}"
