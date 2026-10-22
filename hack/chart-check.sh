#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Render-only checks for charts/koinon (plan item H1): helm lint and helm template for every
# value set in charts/koinon/ci/ (each postgres.mode x gateway.mode), then kubeconform on the
# output. Never talks to a cluster.
set -euo pipefail

cd "$(dirname "$0")/.."

CHART=charts/koinon
KUBECONFORM_VERSION=v0.8.0
KUBERNETES_VERSION=1.37.0
# CRD schemas (CNPG Cluster) from the datree catalog, pinned to a commit.
CRDS_CATALOG=501527ab9c40c827144c8f45945b25a023e344ce

HELM=${HELM:-helm}
KUBECONFORM=(go run "github.com/yannh/kubeconform/cmd/kubeconform@${KUBECONFORM_VERSION}")

out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

value_files=("$CHART/values.yaml" "$CHART"/ci/*-values.yaml)

for f in "${value_files[@]}"; do
	name=$(basename "$f" .yaml)
	echo "== $name"
	"$HELM" lint --strict "$CHART" -f "$f"
	"$HELM" template koinon "$CHART" --namespace koinon -f "$f" >"$out/$name.yaml"
done

# The preflight hook (plan item H2) renders unless preflight.enabled=false, as a pre-install and
# pre-upgrade hook whose output Helm prints; the probe RBAC only with the seccomp probe on.
echo "== preflight hook"
preflight_job() { "$HELM" template koinon "$CHART" --namespace koinon -f "$1" --show-only templates/preflight.yaml 2>/dev/null; }
for f in "${value_files[@]}"; do
	name=$(basename "$f" .yaml)
	enabled=$("$HELM" template koinon "$CHART" -f "$f" --show-only templates/preflight.yaml >/dev/null 2>&1 && echo true || echo false)
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
	probe=$(grep -c -- '--seccomp-probe=true' <<<"$rendered" || true)
	role=$(grep -c '^kind: Role$' <<<"$rendered" || true)
	[[ $probe == "$role" ]] || { echo "$name: probe Role present=$role but probe enabled=$probe" >&2; exit 1; }
	echo "$name: ok (seccomp probe: $([[ $probe == 1 ]] && echo on || echo off))"
done

echo "== kubeconform (Kubernetes $KUBERNETES_VERSION)"
"${KUBECONFORM[@]}" \
	-strict -summary \
	-kubernetes-version "$KUBERNETES_VERSION" \
	-schema-location default \
	-schema-location "https://raw.githubusercontent.com/datreeio/CRDs-catalog/${CRDS_CATALOG}/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json" \
	"$out"/*.yaml
