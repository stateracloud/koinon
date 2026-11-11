#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Render-only checks for charts/koinon (plan item H1): helm lint and helm template for every
# value set in charts/koinon/ci/ (each postgres.mode x gateway.mode), assertions on the rendered
# output for chosen values, then kubeconform on the output. Never talks to a cluster.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

cd "$(dirname "$0")/.."

CHART=charts/koinon
# shellcheck source=/dev/null
. hack/versions.env # KUBECONFORM_VERSION
KUBERNETES_VERSION=1.37.0
# CRD schemas (CNPG Cluster) from the datree catalog, pinned to a commit.
CRDS_CATALOG=501527ab9c40c827144c8f45945b25a023e344ce

HELM=${HELM:-helm}
KUBECONFORM=(go run "github.com/yannh/kubeconform/cmd/kubeconform@${KUBECONFORM_VERSION}")

# helm >= 4: --dry-run=client under helm 3 still contacts the cluster and fails with
# "Kubernetes cluster unreachable" when KUBECONFIG=/dev/null, which the NOTES assertions then
# misreport as missing text (#349 / old #420). CI pins HELM_VERSION in hack/versions.env.
# Keep stderr: a sandbox EPERM or missing binary must reach the log so lane-gates can mark
# chart notrun (exit 3), not fail with a false "need helm 4" (#349 Bugbot).
helm_ver=""
helm_err=0
helm_ver=$("$HELM" version --template '{{.Version}}' 2>&1) || helm_err=$?
if [[ $helm_err -ne 0 ]]; then
	echo "chart-check: cannot run $HELM (exit $helm_err): ${helm_ver:-no output}" >&2
	exit 3
fi
helm_major=${helm_ver#v}
helm_major=${helm_major%%.*}
if ! [[ ${helm_major:-} =~ ^[0-9]+$ ]] || ((helm_major < 4)); then
	echo "chart-check: need helm 4 or newer (got ${helm_ver:-unknown}); CI pins HELM_VERSION=$HELM_VERSION" >&2
	exit 1
fi

out=$(mktemp -d "${TMPDIR:-/tmp}/chart-check.XXXXXX")
trap 'rm -rf "$out"' EXIT

value_files=("$CHART/values.yaml" "$CHART"/ci/*-values.yaml)
# The chart requires openshell.clientTLSSecret or openshell.insecure=true (old issue 103); the default
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
# fail records into a file, not only a counter: an assertion that runs in a subshell (a pipe, $(...)
# or a ( ... ) group) cannot bump a variable of this shell, and a FAIL that does not reach the exit
# code makes every assertion advisory (#323).
failure_log="$out/failures"
: >"$failure_log"
fail() {
	echo "FAIL: $*" >&2
	echo "$*" >>"$failure_log"
}
# gate_failures exits non-zero when any assertion recorded a failure, from any shell level.
gate_failures() {
	local n
	n=$(wc -l <"$failure_log" | tr -d ' ')
	if [ "$n" -gt 0 ]; then
		echo "$n assertion(s) failed" >&2
		exit 1
	fi
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
# CHART_CHECK_D points the loop at another directory (hack/chart-check-test.sh plants failures there).
for part in "${CHART_CHECK_D:-hack/chart-check.d}"/*.sh; do
	# shellcheck source=/dev/null
	source "$part"
done
gate_failures
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
