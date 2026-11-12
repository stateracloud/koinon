# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the schema-version readiness gate (ADR 0085, old issue 408).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# Each binary answers /readyz from the schema gate and keeps /healthz for liveness, so a pod on a
# schema outside its goose range waits (not Ready) instead of restarting.
deployments=$(render --show-only templates/deployments.yaml)
probe_path() { # component probe: the httpGet path of that probe of the component's container
	awk -v c="component: $1" -v p="$2:" 'BEGIN{RS="\n---\n"} index($0, c "\n") {n=split($0, l, "\n"); for(i=1;i<=n;i++) if (l[i] ~ "^ +" p "$") {sub(/.*path: /, "", l[i+1]); sub(/,.*/, "", l[i+1]); print l[i+1]; exit}}' <<<"$deployments"
}
for c in api agentd reconciler; do
	[[ $(probe_path "$c" readinessProbe) == /readyz ]] || fail "$c readinessProbe is /readyz (schema gate), got '$(probe_path "$c" readinessProbe)'"
	[[ $(probe_path "$c" livenessProbe) == /healthz ]] || fail "$c livenessProbe stays /healthz, got '$(probe_path "$c" livenessProbe)'"
done
awk 'BEGIN{RS="\n---\n"} index($0, "component: agentd\n")' <<<"$deployments" | hit -F -- "- name: health" || fail "agentd exposes its health port"
