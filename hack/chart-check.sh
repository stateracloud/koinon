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
# bootstrapAdmin.enabled=false turns password login off and renders no password.
env_is KOINON_BOOTSTRAP_LOGIN '"false"' --set bootstrapAdmin.enabled=false
lacks "no bootstrap Secret when bootstrapAdmin is disabled" 'name: koinon-bootstrap-admin$' --set bootstrapAdmin.enabled=false
lacks "no password file when bootstrapAdmin is disabled" 'KOINON_BOOTSTRAP_PASSWORD_FILE' --set bootstrapAdmin.enabled=false
lacks "bootstrap login stays on by default" 'KOINON_BOOTSTRAP_LOGIN'
# bootstrapAdmin.username reaches koinon-api.
env_is KOINON_BOOTSTRAP_ADMIN_USERNAME '"admin"'
env_is KOINON_BOOTSTRAP_ADMIN_USERNAME '"ops"' --set bootstrapAdmin.username=ops
# bootstrapAdmin.existingSecret: no generated Secret, the API mounts the given one.
# The chart renders no password: the ensure-secrets hook Job creates the missing Secrets.
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
# port exposes it (the config the reconciler renders keeps it on localhost: internal/gateway/standalone tests).
for f in "${value_files[@]}"; do
	if grep -Eq -- '(containerPort|port|targetPort): 15000$' "$out/$(basename "$f" .yaml).yaml"; then
		fail "port 15000 exposed with $(basename "$f")"
	fi
done
# The key-encryption keys (plan item PV1): a generated Secret kept on uninstall, or an existing one;
# mounted into koinon-api and koinon-reconciler only, never into agentd or the gateway.
# The chart renders no key material (a render makes new keys every time): koinon-api creates the Secret.
for f in "${value_files[@]}"; do
	if grep -Eq -- 'kek-[0-9]{8}' "$out/$(basename "$f" .yaml).yaml"; then fail "key material rendered with $(basename "$f")"; fi
done
env_is KOINON_KEK_SECRET '"koinon-kek"'
env_is KOINON_KEK_CREATE '"true"'
env_is KOINON_KEK_CREATE '"false"' --set secrets.kek.existingSecret=mine
env_is KOINON_KEK_SECRET '"mine"' --set secrets.kek.existingSecret=mine
has "existing KEK Secret mounted" 'secretName: mine$' --set secrets.kek.existingSecret=mine --show-only templates/deployments.yaml
n=$(render --show-only templates/deployments.yaml | grep -c 'mountPath: /secrets/kek$' || true)
[ "$n" = 1 ] || fail "KEK mounted into $n components, want only koinon-reconciler"
# koinon-agentd holds no KEK; it posts a stuck sandbox's work to koinon-api's tools port (ADR 0054).
env_is KOINON_SAVED_WORK_URL '"http://koinon-api-saved-work.koinon.svc.cluster.local:8082"'
# The URL agentd posts to must name a Service that exists, on the port it exposes.
svc=$(render --show-only templates/deployments.yaml | awk '/^kind: Service$/{s=1} s&&/^  name: koinon-api-saved-work$/{f=1} f&&/port: 8082$/{p=1} /^---$/{s=0;f=0} END{exit !p}' && echo yes || echo no)
[ "$svc" = yes ] || fail "no Service koinon-api-saved-work on port 8082 for KOINON_SAVED_WORK_URL"
env_is KOINON_SAVED_WORK_KEY_FILE '/secrets/saved-work/key'
n=$(render --show-only templates/deployments.yaml | grep -c 'mountPath: /secrets/saved-work$' || true)
[ "$n" = 1 ] || fail "the saved-work key is mounted into $n components, want only koinon-agentd"
# The saved-work port admits agentd only; the tools port stays agentgateway's alone.
np=$(render --show-only templates/networkpolicy.yaml)
sw=$(printf '%s\n' "$np" | grep -A8 -- '- port: savedwork' || true)
printf '%s\n' "$sw" | grep -q 'component: agentd' || fail "the saved-work port does not admit koinon-agentd"
printf '%s\n' "$sw" | grep -q 'component: gateway' && fail "the saved-work port admits the gateway"
tl=$(printf '%s\n' "$np" | grep -A8 -- '- port: tools' || true)
printf '%s\n' "$tl" | grep -q 'component: agentd' && fail "the tools port admits koinon-agentd"
# koinon-api cannot delete Secrets unless the legacy connection migration is on.
lacks "no Secret delete by default" 'verbs: \[delete\]'
has "Secret delete with the legacy migration" 'verbs: \[delete\]' --set secrets.migrateLegacyConnections=true
n=$(render --show-only templates/gateway-standalone.yaml | grep -c 'kek' || true)
[ "$n" = 0 ] || fail "the gateway references the KEK Secret"
# The gateway mounts the provider keys Secret, and that Secret only.
has "gateway mounts the provider keys" 'mountPath: /secrets/providers$' --show-only templates/gateway-standalone.yaml
n=$(render | grep -c 'secretName: koinon-gateway-providers$' || true)
[ "$n" = 1 ] || fail "the provider keys Secret is mounted by $n pods, want only the gateway"
# The gateway's egress is restricted by a standard NetworkPolicy; internal upstreams are added by value.
has "gateway egress policy" 'policyTypes: \[Ingress, Egress\]' --show-only templates/networkpolicy.yaml
has "gateway egress carves out the private ranges" '10.0.0.0/8' --show-only templates/networkpolicy.yaml
lacks "no egress policy with networkPolicy.enabled=false" 'policyTypes: \[Ingress, Egress\]' --set networkPolicy.enabled=false
has "gatewayEgressTo is rendered" 'cidr: 10.20.0.0/24' --set 'networkPolicy.gatewayEgressTo[0].to[0].ipBlock.cidr=10.20.0.0/24' --show-only templates/networkpolicy.yaml
env_is KOINON_EGRESS_ALLOWED_HOSTS '"ollama.example.com:11434"' --set 'egress.allowedInternalHosts[0]=ollama.example.com:11434'
# The old name still works, and both are joined.
env_is KOINON_EGRESS_ALLOWED_HOSTS '"ollama.example.com:11434"' --set 'providers.allowedInternalHosts[0]=ollama.example.com:11434'
env_is KOINON_EGRESS_ALLOWED_HOSTS '"mcp.lab.example.com,ollama.example.com:11434"' --set 'egress.allowedInternalHosts[0]=mcp.lab.example.com' --set 'providers.allowedInternalHosts[0]=ollama.example.com:11434'
# The reconciler is not ready until it holds the key-encryption keys.
has "reconciler readiness probe" 'path: /readyz' --show-only templates/deployments.yaml
# Provider values are gone: orgs add providers in the UI, the admin API or declarative Secrets.
if render --set model.baseUrl=https://x/v1 >/dev/null 2>&1; then
	fail "model.* values must no longer be accepted"
fi
# Kubernetes gateway mode (plan item G2): the chart renders no gateway of its own, only the Secret the
# reconciler writes provider keys into; a standard NetworkPolicy selects the controller's proxy pods
# by their gateway-name label; the reconciler may manage the agentgateway objects in the release
# namespace and nothing cluster-wide.
k8s=(--set gateway.mode=kubernetes)
lacks "no gateway Deployment in kubernetes mode" 'ghcr.io/agentgateway/agentgateway' "${k8s[@]}"
has "provider keys Secret in kubernetes mode" 'name: koinon-gateway-providers$' "${k8s[@]}" --show-only templates/gateway-kubernetes.yaml
n=$(render "${k8s[@]}" | grep -c 'secretName: koinon-gateway-providers$' || true)
[ "$n" = 0 ] || fail "the provider keys Secret is mounted by $n pods in kubernetes mode, want none (the controller reads it)"
has "proxy NetworkPolicy selects the controller's pod label" 'gateway.networking.k8s.io/gateway-name: koinon-gateway$' "${k8s[@]}" --show-only templates/networkpolicy.yaml
has "proxy egress to the controller's xDS port" 'port: 9978$' "${k8s[@]}" --show-only templates/networkpolicy.yaml
has "proxy egress carves out the private ranges" '10.0.0.0/8' "${k8s[@]}" --show-only templates/networkpolicy.yaml
has "reconciler manages the agentgateway objects" 'agentgatewaymodels' "${k8s[@]}" --show-only templates/rbac.yaml
lacks "no agentgateway rules in standalone mode" 'agentgatewaymodels' --show-only templates/rbac.yaml
cr=$(render "${k8s[@]}" --show-only templates/rbac.yaml | awk '/^kind: ClusterRole$/{p=1} /^---$/{p=0} p')
grep -Eq 'agentgateway|configmaps|gateways' <<<"$cr" && fail "a ClusterRole grants access to gateway objects or ConfigMaps in kubernetes mode"
has "reconciler config names the Gateway" 'name: koinon-gateway$' "${k8s[@]}" --show-only templates/config.yaml
has "reconciler config names the GatewayClass" 'gatewayClassName: agentgateway$' "${k8s[@]}" --show-only templates/config.yaml
has "sandboxes reach the Gateway's Service" 'http://koinon-gateway.koinon.svc.cluster.local:4000' "${k8s[@]}" --show-only templates/deployments.yaml
env_is KOINON_GATEWAY_MCP_URL '"http://koinon-gateway.koinon.svc.cluster.local:4000/mcp"' "${k8s[@]}"
has "preflight checks the agentgateway controller" '--agentgateway-controller=agentgateway$' "${k8s[@]}" --show-only templates/preflight.yaml
has "preflight may read the agentgateway CRDs" 'agentgatewaymodels.agentgateway.dev$' "${k8s[@]}" --show-only templates/preflight.yaml
env_is KOINON_GATEWAY_MCP_URL '"http://koinon-gateway.koinon.svc.cluster.local:8080/mcp"'
has "gateway.kubernetes.port is used" 'url: http://koinon-gateway.koinon.svc.cluster.local:4001$' "${k8s[@]}" --set gateway.kubernetes.port=4001 --show-only templates/config.yaml
# GitOps-safe renders (issue #98). (a) Two renders of every value set are byte-identical.
for f in "${value_files[@]}"; do
	name=$(basename "$f" .yaml)
	"$HELM" template koinon "$CHART" --namespace koinon -f "$f" >"$out/$name.again"
	cmp -s "$out/$name.yaml" "$out/$name.again" || fail "two renders of $name differ"
	rm -f "$out/$name.again"
done
# (b) No lookup remains: a render must not depend on the cluster.
if grep -rnE '\blookup\b' "$CHART/templates"; then fail "a template uses lookup"; fi
# kinds_named <kind> <helm args...>: the names of the objects of that kind in the rendering.
kinds_named() {
	local kind=$1
	shift
	render "$@" | awk -v kind="$kind" '
		/^---$/ { if (k == kind && n != "") print n; k = ""; n = ""; meta = 0; next }
		/^kind: / { k = $2 }
		/^metadata:/ { meta = 1; next }
		meta && /^  name: / && n == "" { n = $2 }
		/^[a-z]/ && !/^metadata:/ { meta = 0 }
		END { if (k == kind && n != "") print n }'
}
# (c) cnpg mode: the hook Job exists and no Secret is rendered for the roles or the bootstrap admin.
has "ensure-secrets hook Job in cnpg mode" '^            - ensure-secrets$' --show-only templates/ensure-secrets.yaml
has "ensure-secrets is a pre-install,pre-upgrade hook" 'helm.sh/hook: pre-install,pre-upgrade' --show-only templates/ensure-secrets.yaml
# Argo CD's label tracking would claim (and prune) Secrets that carry the instance label.
lacks "generated Secrets carry no app.kubernetes.io/instance label" '--secrets=.*app\.kubernetes\.io/instance' --show-only templates/ensure-secrets.yaml --set postgres.mode=cnpg
secrets=$(kinds_named Secret | tr '\n' ' ')
[[ $secrets == *koinon-db-* || $secrets == *koinon-bootstrap-admin* ]] && fail "cnpg mode renders Secrets $secrets"
for want in koinon-db-migrate koinon-db-api koinon-db-reconciler koinon-db-agentd koinon-bootstrap-admin; do
	has "the hook Job ensures $want" "$want" --show-only templates/ensure-secrets.yaml
done
# (d) With every Secret supplied there is no hook Job, and the given names are referenced.
own=(--set bootstrapAdmin.existingSecret=mine-admin
	--set postgres.cnpg.existingSecrets.migrate=mine-migrate --set postgres.cnpg.existingSecrets.api=mine-api
	--set postgres.cnpg.existingSecrets.reconciler=mine-reconciler --set postgres.cnpg.existingSecrets.agentd=mine-agentd)
if render "${own[@]}" --show-only templates/ensure-secrets.yaml >/dev/null 2>&1; then
	fail "the ensure-secrets hook renders although every Secret is supplied"
fi
lacks "no ensure-secrets Job with existing Secrets" 'ensure-secrets' "${own[@]}"
for name in mine-migrate mine-api mine-reconciler mine-agentd; do
	has "$name is referenced" "name: $name\$" "${own[@]}"
done
lacks "the generated role Secret names are unused with existing Secrets" 'koinon-db-' "${own[@]}"
has "the Cluster's initdb takes the given migrate Secret" 'name: mine-migrate$' "${own[@]}" --show-only templates/postgres-cnpg.yaml
# Argo CD (#118, ADR 0060): the cnpg install Job is a Sync hook in wave 1 recreated each sync, and what
# must follow it (the Deployments, the standalone gateway, which needs the reconciler's ConfigMap) is in wave 2.
mig=$(render --show-only templates/migrate-job.yaml)
for want in 'argocd.argoproj.io/hook: Sync' 'argocd.argoproj.io/hook-delete-policy: BeforeHookCreation' 'argocd.argoproj.io/sync-wave: "1"'; do
	grep -qF -- "$want" <<<"$mig" || fail "the cnpg migration Job lacks '$want'"
done
lacks "the external-mode migration hook has no Argo annotations" 'argocd.argoproj.io' --set postgres.mode=external --set postgres.external.secretName=db --show-only templates/migrate-job.yaml
n=$(render --show-only templates/deployments.yaml | grep -c 'argocd.argoproj.io/sync-wave: "2"' || true)
[ "$n" = 3 ] || fail "$n of the 3 koinon Deployments are in sync wave 2"
n=$(render --show-only templates/gateway-standalone.yaml | grep -c 'argocd.argoproj.io/sync-wave: "2"' || true)
[ "$n" = 2 ] || fail "$n of the gateway's PVC and Deployment are in sync wave 2, want 2"
has "the migration stays a plain Job on install" 'name: koinon-migrate-install$' --show-only templates/migrate-job.yaml
has "the migration Job reads the given migrate Secret" 'name: mine-migrate$' "${own[@]}" --show-only templates/migrate-job.yaml
# One role supplied: the hook ensures the rest only.
lacks "the hook leaves out a supplied role" 'koinon-db-api' --set postgres.cnpg.existingSecrets.api=mine-api --show-only templates/ensure-secrets.yaml
has "the hook still ensures the others" 'koinon-db-migrate' --set postgres.cnpg.existingSecrets.api=mine-api --show-only templates/ensure-secrets.yaml
# External Postgres with a given admin Secret: no Job either.
if render --set postgres.mode=external --set postgres.external.secretName=dsns --set bootstrapAdmin.existingSecret=mine-admin \
	--show-only templates/ensure-secrets.yaml >/dev/null 2>&1; then
	fail "the ensure-secrets hook renders in external mode with an existing admin Secret"
fi
has "external mode still ensures the bootstrap admin" 'koinon-bootstrap-admin' --set postgres.mode=external \
	--set postgres.external.secretName=dsns --show-only templates/ensure-secrets.yaml
# The hook Job reads exactly the Secrets it ensures and creates; it updates and deletes nothing.
lacks "ensure-secrets can update or delete" 'verbs: \[(update|delete|patch)' --show-only templates/ensure-secrets.yaml
# (e) Standalone mode renders no gateway config ConfigMap (the reconciler creates it); the Deployment mounts it.
cms=$(kinds_named ConfigMap --set gateway.mode=standalone | tr '\n' ' ')
[[ $cms == *gateway-config* ]] && fail "standalone mode renders the gateway ConfigMap ($cms)"
has "the gateway mounts its config by name" 'name: koinon-gateway-config$' --set gateway.mode=standalone --show-only templates/gateway-standalone.yaml
has "the reconciler may create the config and read the gateway Deployment" 'resourceNames: \[koinon-gateway\]' --set gateway.mode=standalone --show-only templates/rbac.yaml
has "the reconciler is told the gateway Deployment" 'deployment: koinon-gateway$' --set gateway.mode=standalone --show-only templates/config.yaml
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
