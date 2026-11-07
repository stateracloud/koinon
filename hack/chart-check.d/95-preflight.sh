# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the preflight hook. (The failure gate is in hack/chart-check.sh, after every file.)
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# The preflight hook (plan item H2) renders unless preflight.enabled=false, as a pre-install and
# pre-upgrade hook whose output Helm prints; the probe RBAC only with the seccomp probe on.
echo "== preflight hook"
preflight_job() { "$HELM" template koinon "$CHART" --namespace koinon -f "$1" $(insecure_for "$1") --show-only templates/preflight.yaml 2>/dev/null; }
for f in "${value_files[@]}"; do
	name=$(basename "$f" .yaml)
	enabled=$("$HELM" template koinon "$CHART" -f "$f" $(insecure_for "$f") --show-only templates/preflight.yaml >/dev/null 2>&1 && echo true || echo false)
	if [[ $name == external-standalone-values ]]; then
		[[ $enabled == false ]] || { echo "$name: preflight.enabled=false still renders the hook" >&2; exit 1; }
		continue
	fi
	[[ $enabled == true ]] || { echo "$name: preflight hook not rendered" >&2; exit 1; }
	rendered=$(preflight_job "$f")
	for want in 'kind: Job' 'helm.sh/hook: pre-install,pre-upgrade' 'helm.sh/hook-output-log-policy: hook-failed,hook-succeeded' \
		'kind: ClusterRole' '- preflight'; do
		grep -qF -- "$want" <<<"$rendered" || { echo "$name: preflight hook lacks '$want'" >&2; exit 1; }
	done
	# The ClusterRole is get-only, except that in cnpg mode the CNPG operator check lists
	# exactly validatingwebhookconfigurations and endpointslices (neither can be narrowed by
	# resourceNames).
	clusterrole=$(sed -n '/kind: ClusterRole$/,/^---/p' <<<"$rendered")
	if grep -qE '^ *verbs:' <<<"$clusterrole" && grep -E '^ *verbs:' <<<"$clusterrole" | grep -qvE '^ *verbs: \[(get|list)\]$'; then
		echo "$name: preflight ClusterRole has verbs other than get and list" >&2
		exit 1
	fi
	listed=$(awk '/^ *resources:/ { res = $0; sub(/^ *resources: */, "", res) } /^ *verbs: \[list\]$/ { print res }' <<<"$clusterrole" | sort | tr '\n' ' ')
	want_listed=""
	[[ $name == external-* ]] || want_listed="[endpointslices] [validatingwebhookconfigurations] "
	if [[ $listed != "$want_listed" ]]; then
		echo "$name: preflight ClusterRole lists '${listed}', want '${want_listed}' (get-only otherwise)" >&2
		exit 1
	fi
	# The probes' Role is namespaced (the release namespace only) and renders iff a probe is on:
	# pods create/get/delete and pods/log get for either probe, plus networkpolicies
	# create/delete for the NetworkPolicy probe and nothing else.
	probe=$(grep -c -- '--seccomp-probe=true' <<<"$rendered" || true)
	netpol=$(grep -c -- '--netpol-probe=true' <<<"$rendered" || true)
	role=$(grep -c '^kind: Role$' <<<"$rendered" || true)
	want_role=0
	[[ $probe == 1 || $netpol == 1 ]] && want_role=1
	[[ $role == "$want_role" ]] || { echo "$name: probe Role present=$role, want $want_role (seccomp=$probe netpol=$netpol)" >&2; exit 1; }
	if [[ $role == 1 ]]; then
		rolerules=$(sed -n '/^kind: Role$/,/^---/p' <<<"$rendered" | awk '/^ *resources:/ { res = $0; sub(/^ *resources: */, "", res) } /^ *verbs:/ { v = $0; sub(/^ *verbs: */, "", v); print res " " v }' | LC_ALL=C sort | tr '\n' ';')
		want_rules="[pods/log] [get];[pods] [create, get, delete];"
		[[ $netpol == 1 ]] && want_rules="[networkpolicies] [create, delete];$want_rules"
		if [[ $rolerules != "$want_rules" ]]; then
			echo "$name: preflight Role rules '${rolerules}', want '${want_rules}'" >&2
			exit 1
		fi
		grep -q '^  namespace:' <<<"$(sed -n '/^kind: Role$/,/^---/p' <<<"$rendered")" &&
			{ echo "$name: preflight Role names a namespace; it must be the release namespace" >&2; exit 1; }
	fi
	echo "$name: ok (seccomp probe: $([[ $probe == 1 ]] && echo on || echo off), NetworkPolicy probe: $([[ $netpol == 1 ]] && echo on || echo off))"
done

