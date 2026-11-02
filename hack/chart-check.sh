#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Render-only checks for charts/koinon (plan item H1): helm lint and helm template for every
# value set in charts/koinon/ci/ (each postgres.mode x gateway.mode), assertions on the rendered
# output for chosen values, then kubeconform on the output. Never talks to a cluster.
set -euo pipefail

cd "$(dirname "$0")/.."

CHART=charts/koinon
KUBECONFORM_VERSION=v0.8.0
KUBERNETES_VERSION=1.37.0
# CRD schemas (CNPG Cluster) from the datree catalog, pinned to a commit.
CRDS_CATALOG=501527ab9c40c827144c8f45945b25a023e344ce

HELM=${HELM:-helm}
KUBECONFORM=(go run "github.com/yannh/kubeconform/cmd/kubeconform@${KUBECONFORM_VERSION}")

out=$(mktemp -d "${TMPDIR:-/tmp}/chart-check.XXXXXX")
trap 'rm -rf "$out"' EXIT

value_files=("$CHART/values.yaml" "$CHART"/ci/*-values.yaml)
# The chart requires openshell.clientTLSSecret or openshell.insecure=true (#103); the default
# values set neither, so they are rendered with insecure, as are the assertions below (render).
# The same goes for openshell.address (required, no default).
# Value files choose for themselves. insecure_for <values file>: the extra helm arguments.
ADDRESS="openshell.address=openshell.openshell.svc:8080"
insecure_for() { [[ $1 == "$CHART/values.yaml" ]] && echo "--set openshell.insecure=true --set $ADDRESS" || true; }

for f in "${value_files[@]}"; do
	name=$(basename "$f" .yaml)
	echo "== $name"
	# shellcheck disable=SC2046
	"$HELM" lint --strict "$CHART" -f "$f" $(insecure_for "$f")
	# shellcheck disable=SC2046
	"$HELM" template koinon "$CHART" --namespace koinon -f "$f" $(insecure_for "$f") >"$out/$name.yaml"
done

# Assertions on rendered output: render with --set flags, then grep.
render() { "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" "$@"; }
# render_raw renders without that default, for the assertions about the TLS choice itself.
render_raw() { "$HELM" template koinon "$CHART" --namespace koinon --set "$ADDRESS" "$@"; }
# Assertions capture the rendering before grepping it: piping into grep -q under pipefail fails
# the pipeline when grep exits on the first match and helm then dies of SIGPIPE.
failures=0
fail() {
	echo "FAIL: $*" >&2
	failures=$((failures + 1))
}
# has <description> <extended regexp> <helm args...>: the rendering matches the pattern.
has() {
	local what=$1 re=$2
	shift 2
	local out
	out=$(render "$@")
	grep -Eq -- "$re" <<<"$out" || fail "$what"
}
# lacks <description> <extended regexp> <helm args...>: the rendering does not match the pattern.
lacks() {
	local what=$1 re=$2
	shift 2
	local out
	out=$(render "$@")
	if grep -Eq -- "$re" <<<"$out"; then fail "$what"; fi
}
# env <name> <value> <helm args...>: koinon-api's env has name=value (the line after the name).
env_is() {
	local name=$1 value=$2
	shift 2
	local out
	out=$(render --show-only templates/deployments.yaml "$@" | grep -A1 -E -- "- name: $name\$") || true
	grep -Fq -- "value: $value" <<<"$out" || fail "env $name=$value with $*"
}

echo "== assertions"
# The assertions live in hack/chart-check.d/NN-<feature>.sh, sourced in order; add a new feature as
# a new file (or append to the one that owns the feature) so concurrent changes do not collide here.
for part in hack/chart-check.d/*.sh; do
	# shellcheck source=/dev/null
	source "$part"
done
# kubeconform's schemas do not make list entries unique; the API server rejects a pod spec
# that repeats a volume, container, mount or env name.
echo "== duplicate list entries"
go run ./hack/chartlint "$out"/*.yaml

echo "== kubeconform (Kubernetes $KUBERNETES_VERSION)"
"${KUBECONFORM[@]}" \
	-strict -summary \
	-kubernetes-version "$KUBERNETES_VERSION" \
	-schema-location default \
	-schema-location "https://raw.githubusercontent.com/datreeio/CRDs-catalog/${CRDS_CATALOG}/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json" \
	"$out"/*.yaml
