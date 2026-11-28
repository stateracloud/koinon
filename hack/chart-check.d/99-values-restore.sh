# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the restore preset charts/koinon/values-restore.yaml (issue #517).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, env_is, ...).

rp=(-f "$CHART/values-restore.yaml")
# The preset holds all three binaries and moves the install to its own gateway ID.
env_is KOINON_RESTORE_HOLD '"true"' "${rp[@]}"
hold_count_p=$(render --show-only templates/deployments.yaml "${rp[@]}" | grep -c -E -- '- name: KOINON_RESTORE_HOLD$' || true)
[[ $hold_count_p == 3 ]] || fail "values-restore.yaml holds the three Deployments, got $hold_count_p"
has "preset names its own gateway ID" 'gatewayId: "koinon-restore"' --show-only templates/config.yaml "${rp[@]}"
lacks "preset does not use the shared gateway ID" 'gatewayId: "openshell"' --show-only templates/config.yaml "${rp[@]}"
# The caller's later values file wins over the preset.
has "a later values file overrides the preset's gateway ID" 'gatewayId: "my-gw"' --show-only templates/config.yaml "${rp[@]}" --set openshell.gatewayId=my-gw
# The preset leaves the OpenShell address to the caller: with the openshell driver and without it the chart
# still refuses to render. (With the default driver, substrate, the preset renders, and its gateway ID has no
# meaning for Substrate: a held restore on the same cluster has no isolation there, issue #1548. The preset and
# backup-restore.e1 carry the interim warning until #1548 closes.)
if "$HELM" template koinon "$CHART" --namespace koinon --set execution.driver=openshell --set sandboxObserver.enabled=true --set sandboxNetworkPolicy.enabled=true --set sandboxLimitRange.enabled=true --set openshell.insecure=true -f "$CHART/values-restore.yaml" >/dev/null 2>&1; then
	fail "values-restore.yaml renders without openshell.address: the new gateway's address must stay the caller's"
fi

# NOTES.txt prints the object names a restore needs (issue #517). NOTES come from install, not template.
restore_notes() {
	local out rc=0
	out=$("$HELM" install koinon "$CHART" -n restored --dry-run=client "${OPENSHELL_ARGS[@]}" --set openshell.insecure=true --set "$ADDRESS" "$@" 2>&1) || rc=$?
	if [[ $rc -ne 0 ]]; then
		fail "helm install --dry-run=client for NOTES failed: $(head -1 <<<"$out")"
		return 0
	fi
	printf '%s\n' "$out"
}
n_cnpg=$(restore_notes --set postgres.mode=cnpg)
grep -q 'NS=restored REL=koinon' <<<"$n_cnpg" || fail "NOTES do not give the guide's variables"
grep -q 'Deployments: koinon-api, koinon-reconciler, koinon-agentd' <<<"$n_cnpg" || fail "NOTES do not name the Deployments"
grep -q 'KEK Secret:  koinon-kek' <<<"$n_cnpg" || fail "NOTES do not name the KEK Secret"
grep -q 'cnpg.io/cluster=koinon-pg' <<<"$n_cnpg" || fail "NOTES do not name the CloudNativePG cluster"
grep -q 'HELD (restoreHold=true' <<<"$n_cnpg" && fail "NOTES say HELD on an install that is not held"
n_held=$(restore_notes --set postgres.mode=cnpg "${rp[@]}")
grep -q 'HELD (restoreHold=true' <<<"$n_held" || fail "NOTES do not say a restoreHold install is held"
n_ext=$(restore_notes --set postgres.mode=external --set postgres.external.secretName=koinon-db)
grep -q 'Database:    CloudNativePG' <<<"$n_ext" && fail "NOTES name a CloudNativePG cluster for an external database"
n_full=$(restore_notes --set fullnameOverride=other)
grep -q 'named after other, not the release' <<<"$n_full" || fail "NOTES do not warn when the fullname differs from the release"
# Issue #1548 interim: the preset and the backup guide warn that a held restore on the same cluster has no
# isolation on Substrate. Remove these two checks with the warnings when #1548 closes.
for w in "$CHART/values-restore.yaml" test/e2e/commands/backup-restore.e1; do
	hit 'issues/1548' "$w" || fail "$w lost the #1548 warning (same-cluster restore on Substrate)"
done
