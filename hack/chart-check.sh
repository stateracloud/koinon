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

out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

value_files=("$CHART/values.yaml" "$CHART"/ci/*-values.yaml)

for f in "${value_files[@]}"; do
	name=$(basename "$f" .yaml)
	echo "== $name"
	"$HELM" lint --strict "$CHART" -f "$f"
	"$HELM" template koinon "$CHART" --namespace koinon -f "$f" >"$out/$name.yaml"
done

# Assertions on rendered output: render with --set flags, then grep.
render() { "$HELM" template koinon "$CHART" --namespace koinon "$@"; }
failures=0
fail() {
	echo "FAIL: $*" >&2
	failures=$((failures + 1))
}
# has <description> <extended regexp> <helm args...>: the rendering matches the pattern.
has() {
	local what=$1 re=$2
	shift 2
	render "$@" | grep -Eq -- "$re" || fail "$what"
}
# lacks <description> <extended regexp> <helm args...>: the rendering does not match the pattern.
lacks() {
	local what=$1 re=$2
	shift 2
	if render "$@" | grep -Eq -- "$re"; then fail "$what"; fi
}
# env <name> <value> <helm args...>: koinon-api's env has name=value (the line after the name).
env_is() {
	local name=$1 value=$2
	shift 2
	render --show-only templates/deployments.yaml "$@" |
		grep -A1 -E -- "- name: $name\$" | grep -Fq -- "value: $value" ||
		fail "env $name=$value with $*"
}

echo "== assertions"
# bootstrapAdmin.enabled=false turns password login off and renders no password.
env_is KOINON_BOOTSTRAP_LOGIN '"false"' --set bootstrapAdmin.enabled=false
lacks "no bootstrap Secret when bootstrapAdmin is disabled" 'name: koinon-bootstrap-admin$' --set bootstrapAdmin.enabled=false
lacks "no password file when bootstrapAdmin is disabled" 'KOINON_BOOTSTRAP_PASSWORD_FILE' --set bootstrapAdmin.enabled=false
lacks "bootstrap login stays on by default" 'KOINON_BOOTSTRAP_LOGIN'
# bootstrapAdmin.username reaches koinon-api.
env_is KOINON_BOOTSTRAP_ADMIN_USERNAME '"admin"'
env_is KOINON_BOOTSTRAP_ADMIN_USERNAME '"ops"' --set bootstrapAdmin.username=ops
# bootstrapAdmin.existingSecret: no generated Secret, the API mounts the given one.
has "generated bootstrap Secret by default" 'name: koinon-bootstrap-admin$' --show-only templates/bootstrap-admin.yaml
lacks "no generated bootstrap Secret with existingSecret" 'name: koinon-bootstrap-admin$' --set bootstrapAdmin.existingSecret=mine
has "existingSecret mounted" 'secretName: mine$' --set bootstrapAdmin.existingSecret=mine
# publicUrl: explicit, or derived from the ingress.
lacks "no public URL without ingress or publicUrl" 'KOINON_PUBLIC_URL'
env_is KOINON_PUBLIC_URL '"http://koinon.lan"' --set publicUrl=http://koinon.lan/
env_is KOINON_PUBLIC_URL '"http://koinon.example.com"' --set ingress.enabled=true --set ingress.host=koinon.example.com
env_is KOINON_PUBLIC_URL '"https://koinon.example.com"' --set ingress.enabled=true --set ingress.host=koinon.example.com \
	--set ingress.tlsSecretName=tls
# oidc values map to koinon-api's --oidc-* flags; the client secret is a mounted file.
oidc=(--set publicUrl=https://koinon.example.com --set oidc.enabled=true --set oidc.issuerUrl=https://idp.example.com
	--set oidc.clientId=koinon --set oidc.clientSecret.existingSecret=koinon-oidc --set oidc.groupMappings.staff=member)
lacks "no OIDC env when oidc is disabled" 'KOINON_OIDC_'
env_is KOINON_OIDC_ISSUER_URL '"https://idp.example.com"' "${oidc[@]}"
env_is KOINON_OIDC_CLIENT_ID '"koinon"' "${oidc[@]}"
env_is KOINON_OIDC_CLIENT_SECRET_FILE /etc/koinon/oidc/client-secret "${oidc[@]}"
env_is KOINON_OIDC_SCOPES '"email,profile"' "${oidc[@]}"
env_is KOINON_DEFAULT_ORG_GROUP_MAPPINGS '"staff=member"' "${oidc[@]}"
env_is KOINON_SESSION_MAX_LIFETIME '"12h"' "${oidc[@]}"
has "OIDC client secret mounted" 'secretName: koinon-oidc$' "${oidc[@]}"
if render --set oidc.enabled=true --set oidc.issuerUrl=https://idp.example.com --set oidc.clientId=koinon \
	>/dev/null 2>&1; then
	fail "oidc without a redirect or public URL must not render"
fi
# config.yaml is the reconciler's alone.
n=$(render --show-only templates/deployments.yaml | grep -c 'KOINON_CONFIG' || true)
[ "$n" = 1 ] || fail "KOINON_CONFIG set on $n components, want only the reconciler"
n=$(render --show-only templates/deployments.yaml | grep -c 'mountPath: /etc/koinon/config$' || true)
[ "$n" = 1 ] || fail "config.yaml mounted into $n components, want only the reconciler"
# agentgateway's admin listener (config dump and UI) binds to localhost: no Service or container
# port exposes it, and the bootstrap config keeps it there.
for f in "${value_files[@]}"; do
	if grep -Eq -- '(containerPort|port|targetPort): 15000$' "$out/$(basename "$f" .yaml).yaml"; then
		fail "port 15000 exposed with $(basename "$f")"
	fi
done
has "agentgateway admin listener on localhost" 'adminAddr: localhost:15000$' --set gateway.mode=standalone \
	--show-only templates/gateway-standalone.yaml
if [ "$failures" -gt 0 ]; then
	echo "$failures assertion(s) failed" >&2
	exit 1
fi

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
