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
# The chart requires openshell.clientTLSSecret or openshell.insecure=true (#103); the default
# values set neither, so they are rendered with insecure, as are the assertions below (render).
# Value files choose for themselves. insecure_for <values file>: the extra helm arguments.
insecure_for() { [[ $1 == "$CHART/values.yaml" ]] && echo "--set openshell.insecure=true" || true; }

for f in "${value_files[@]}"; do
	name=$(basename "$f" .yaml)
	echo "== $name"
	# shellcheck disable=SC2046
	"$HELM" lint --strict "$CHART" -f "$f" $(insecure_for "$f")
	# shellcheck disable=SC2046
	"$HELM" template koinon "$CHART" --namespace koinon -f "$f" $(insecure_for "$f") >"$out/$name.yaml"
done

# Assertions on rendered output: render with --set flags, then grep.
render() { "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true "$@"; }
# render_raw renders without that default, for the assertions about the TLS choice itself.
render_raw() { "$HELM" template koinon "$CHART" --namespace koinon "$@"; }
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
# httpRoute: a Gateway API route with the given parentRefs (as written) and hostnames; the public URL
# derives from its first hostname over https.
route=(--set httpRoute.enabled=true --set 'httpRoute.hostnames[0]=koinon.example.com'
	--set-json 'httpRoute.parentRefs=[{"group":"gateway.networking.k8s.io","kind":"Gateway","name":"shared","namespace":"gateway","sectionName":"https"}]')
lacks "no HTTPRoute by default" 'kind: HTTPRoute'
has "HTTPRoute renders with httpRoute.enabled" '^kind: HTTPRoute$' "${route[@]}" --show-only templates/httproute.yaml
has "HTTPRoute keeps the parentRef's group and kind" 'group: gateway.networking.k8s.io' "${route[@]}" --show-only templates/httproute.yaml
has "HTTPRoute keeps the parentRef's sectionName" 'sectionName: https' "${route[@]}" --show-only templates/httproute.yaml
has "HTTPRoute routes to the API Service" 'name: koinon-api$' "${route[@]}" --show-only templates/httproute.yaml
env_is KOINON_PUBLIC_URL '"https://koinon.example.com"' "${route[@]}"
if render_raw --set openshell.insecure=true --set httpRoute.enabled=true >/dev/null 2>&1; then fail "httpRoute without hostnames and parentRefs must not render"; fi
if render "${route[@]}" --set ingress.enabled=true --set ingress.host=koinon.example.com >/dev/null 2>&1; then fail "ingress and httpRoute together must not render"; fi
if render --set httpRoute.enabled=true --set 'httpRoute.hostnames[0]=*.example.com' --set-json 'httpRoute.parentRefs=[{"name":"shared"}]' >/dev/null 2>&1; then fail "a wildcard first hostname without publicUrl must not render"; fi
has "httpRoute.timeouts reach the route's rule" 'backendRequest: 0s' "${route[@]}" --set httpRoute.timeouts.backendRequest=0s --show-only templates/httproute.yaml
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
# The CONNECT policy (pods/exec, attach, portforward, pods/proxy, services/proxy) and its binding
# render in every mode that has the sandbox policies, with no exemption (no variables, no identity list), ADR 0048 decision 3.
for f in "${value_files[@]}"; do
	r="$out/$(basename "$f" .yaml).yaml"
	# A value set that turns the policies off (the cluster-scoped objects need admin rights) has none.
	grep -Eq -- '^kind: ValidatingAdmissionPolicy$' "$r" || { grep -A1 -E '^sandboxPolicy:' "$f" | grep -q 'enabled: false' && continue; }
	grep -Eq -- '^  name: koinon-koinon-sandbox-connect$' "$r" || fail "no CONNECT policy with $(basename "$f")"
	[ "$(grep -c -- '^  name: koinon-koinon-sandbox-connect$' "$r")" = 2 ] || fail "CONNECT policy and binding must both render with $(basename "$f")"
	grep -Eq -- 'operations: \[CONNECT\]' "$r" || fail "no CONNECT rule with $(basename "$f")"
	grep -Eq -- 'resources: \[pods/exec, pods/attach, pods/portforward, pods/proxy, services/proxy\]' "$r" || fail "CONNECT rule lacks a subresource with $(basename "$f")"
done
lacks "no CONNECT policy with sandboxPolicy.enabled=false" 'sandbox-connect' --set sandboxPolicy.enabled=false

# The OpenShell channel needs mTLS (#103): without a client TLS Secret the chart renders only with
# an explicit openshell.insecure=true, never with both, and each choice reaches the components.
if render_raw >/dev/null 2>"$out/tls.err"; then fail "no client TLS Secret and no insecure must not render"; fi
grep -q 'openshell.clientTLSSecret is required' "$out/tls.err" && grep -q 'openshell.insecure=true' "$out/tls.err" || fail "the TLS error must name both openshell.clientTLSSecret and openshell.insecure"
render_raw --set openshell.clientTLSSecret=tls >/dev/null 2>&1 || fail "a client TLS Secret alone must render"
render_raw --set openshell.insecure=true >/dev/null 2>&1 || fail "openshell.insecure=true alone must render"
if render_raw --set openshell.clientTLSSecret=tls --set openshell.insecure=true >"$out/both.out" 2>"$out/both.err"; then fail "a client TLS Secret together with insecure must not render"; fi
grep -q 'contradict' "$out/both.err" || fail "the TLS-plus-insecure error must say the options contradict"
has "TLS Secret reaches the reconciler config" 'tls: /etc/koinon/openshell-tls' --set openshell.insecure=false --set openshell.clientTLSSecret=tls --show-only templates/config.yaml
has "insecure renders an empty tls dir (plaintext, warned by the components)" 'tls: ""' --show-only templates/config.yaml
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
# The DBOS runtime (ADR 0066) is opt-in; its tables come from the migration Job.
lacks "the DBOS runtime is off by default" '"-durable"' --show-only templates/deployments.yaml
lacks "the migration Job leaves the DBOS schema alone by default" '"-durable"' --show-only templates/migrate-job.yaml
has "api.durable.enabled makes the migration Job create the DBOS tables" '"-durable", "migrate"' --set api.durable.enabled=true --show-only templates/migrate-job.yaml
has "api.durable.enabled starts the DBOS runtime" '"-durable"' --set api.durable.enabled=true --show-only templates/deployments.yaml
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
# The reconciler never talks to the agents.x-k8s.io API (sandboxes go through the OpenShell gateway),
# so no rule in any mode grants it Sandbox permissions, and the chart has no ClusterRole for it.
lacks "no Sandbox permissions in the reconciler's RBAC" 'x-k8s\.io|sandboxes' --show-only templates/rbac.yaml
lacks "no Sandbox permissions in the reconciler's RBAC (gateway.mode=kubernetes)" 'x-k8s\.io|sandboxes' "${k8s[@]}" --show-only templates/rbac.yaml
lacks "no ClusterRole in rbac.yaml" '^kind: ClusterRole' --show-only templates/rbac.yaml
has "reconciler config names the Gateway" 'name: koinon-gateway$' "${k8s[@]}" --show-only templates/config.yaml
has "reconciler config names the GatewayClass" 'gatewayClassName: agentgateway$' "${k8s[@]}" --show-only templates/config.yaml
has "sandboxes reach the Gateway's Service" 'http://koinon-gateway.koinon.svc.cluster.local:4000' "${k8s[@]}" --show-only templates/deployments.yaml
env_is KOINON_GATEWAY_MCP_URL '"http://koinon-gateway.koinon.svc.cluster.local:4000/mcp"' "${k8s[@]}"
has "preflight checks the agentgateway controller" '--agentgateway-controller=agentgateway$' "${k8s[@]}" --show-only templates/preflight.yaml
has "preflight may read the agentgateway CRDs" 'agentgatewaymodels.agentgateway.dev$' "${k8s[@]}" --show-only templates/preflight.yaml
env_is KOINON_GATEWAY_MCP_URL '"http://koinon-gateway.koinon.svc.cluster.local:8080/mcp"'
has "gateway.kubernetes.port is used" 'url: http://koinon-gateway.koinon.svc.cluster.local:4001$' "${k8s[@]}" --set gateway.kubernetes.port=4001 --show-only templates/config.yaml
# Metrics (issue #106, ADR 0023): every component has a "metrics" container port and a Service of its
# own; the ServiceMonitors are off by default and need the CRD; the NetworkPolicies admit the port only
# from metrics.allowFrom.
n=$(render --show-only templates/deployments.yaml | grep -c -- '- name: metrics$' || true)
[ "$n" = 3 ] || fail "the metrics container port is on $n Deployments, want 3"
has "metrics port follows metrics.port" 'containerPort: 9191$' --set metrics.port=9191 --show-only templates/deployments.yaml
env_is KOINON_METRICS_ADDR '":9191"' --set metrics.port=9191
n=$(render --show-only templates/metrics.yaml | grep -c '^kind: Service$' || true)
[ "$n" = 3 ] || fail "$n metrics Services, want 3"
lacks "no ServiceMonitor by default" 'kind: ServiceMonitor'
sm=(--set metrics.serviceMonitor.enabled=true --api-versions monitoring.coreos.com/v1/ServiceMonitor)
n=$(render "${sm[@]}" | grep -c '^kind: ServiceMonitor$' || true)
[ "$n" = 3 ] || fail "$n ServiceMonitors with metrics.serviceMonitor.enabled, want 3"
has "ServiceMonitor interval and labels" 'interval: "30s"' "${sm[@]}" --set metrics.serviceMonitor.interval=30s --set metrics.serviceMonitor.labels.release=prom --show-only templates/metrics.yaml
has "ServiceMonitor extra labels" 'release: prom$' "${sm[@]}" --set metrics.serviceMonitor.labels.release=prom --show-only templates/metrics.yaml
if render --set metrics.serviceMonitor.enabled=true >/dev/null 2>&1; then
	fail "metrics.serviceMonitor.enabled=true must fail without the ServiceMonitor CRD"
fi
n=$(render --set metrics.serviceMonitor.enabled=true --set metrics.serviceMonitor.skipCapabilityCheck=true | grep -c '^kind: ServiceMonitor$' || true)
[ "$n" = 3 ] || fail "$n ServiceMonitors with skipCapabilityCheck and no --api-versions, want 3"
lacks "metrics port closed by default" 'port: metrics$' --show-only templates/networkpolicy.yaml
scrape=(--set 'metrics.allowFrom[0].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name=monitoring')
n=$(render "${scrape[@]}" --show-only templates/networkpolicy.yaml | grep -c 'port: metrics$' || true)
[ "$n" = 3 ] || fail "metrics.allowFrom opens the metrics port in $n policies, want api, reconciler and agentd (3)"
has "metrics.allowFrom peer is rendered" 'kubernetes.io/metadata.name: monitoring$' "${scrape[@]}" --show-only templates/networkpolicy.yaml
# GitOps-safe renders (issue #98). (a) Two renders of every value set are byte-identical.
for f in "${value_files[@]}"; do
	name=$(basename "$f" .yaml)
	"$HELM" template koinon "$CHART" --namespace koinon -f "$f" $(insecure_for "$f") >"$out/$name.again"
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
# agentgateway v1.7 always encrypts MCP sessions: standalone mode gets SESSION_KEY from a generated
# Secret (ensure-secrets), never from a template value; kubernetes mode has no standalone gateway.
has "the hook ensures the gateway session key" 'aes256-key' --show-only templates/ensure-secrets.yaml
has "the gateway reads SESSION_KEY from the generated Secret" 'name: SESSION_KEY' --show-only templates/gateway-standalone.yaml
has "SESSION_KEY comes from the Secret's key" 'name: koinon-gateway-session-key$' --show-only templates/gateway-standalone.yaml
lacks "the hook leaves the session key out in kubernetes mode" 'gateway-session-key' --set gateway.mode=kubernetes --show-only templates/ensure-secrets.yaml
lacks "no SESSION_KEY in kubernetes mode" 'SESSION_KEY' --set gateway.mode=kubernetes
has "an existing session key Secret is referenced" 'name: mine-session$' --set gateway.standalone.sessionKey.existingSecret=mine-session --show-only templates/gateway-standalone.yaml
lacks "an existing session key Secret is not ensured" 'gateway-session-key' --set gateway.standalone.sessionKey.existingSecret=mine-session --show-only templates/ensure-secrets.yaml
# (d) With every Secret supplied there is no hook Job, and the given names are referenced.
own=(--set gateway.standalone.sessionKey.existingSecret=mine-session --set bootstrapAdmin.existingSecret=mine-admin
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
	--set gateway.standalone.sessionKey.existingSecret=mine-session \
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
# Registry mirrors and pull secrets (#130): global.imageRegistry, global.registryMirrors and
# global.imagePullSecrets reach every image the chart runs or names. image_refs lists every image
# reference (image:, imageName:, --probe-image, the supervisor pin) of a render, sorted and unique.
image_refs() {
	render "$@" | grep -E -- '^ *(- )?(image|imageName): |--probe-image=|--supervisor-images=|c\.image in ' |
		grep -Eo -- '([a-z0-9.-]+(:[0-9]+)?/)+[A-Za-z0-9._/-]+:[A-Za-z0-9._-]+(@sha256:[a-f0-9]{64})?' | sort -u
}
agw=ghcr.io/agentgateway/agentgateway:v1.7.0@sha256:60216aafb03d37f94e952ff3d51d2f630a65b10ecaebdc0a864882f92610c45b
pg=ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie@sha256:f7e00138159bb45d70b624ba25e446924cb3ed1bf366d13e936c3f81b914064e
# (a) nothing set: every reference is the upstream one, unchanged.
want=$(printf '%s\n' "$agw" "$pg" ghcr.io/nvidia/openshell/supervisor:0.1.5 ghcr.io/stateracloud/flavor-opencode:0.0.0 \
	ghcr.io/stateracloud/koinon-agentd:0.0.0 ghcr.io/stateracloud/koinon-api:0.0.0 ghcr.io/stateracloud/koinon-reconciler:0.0.0 | sort -u)
[[ $(image_refs) == "$want" ]] || fail "default image references changed: $(image_refs | tr '\n' ' ')"
# (b) imageRegistry replaces every host and keeps path, tag and digest.
want=$(printf '%s\n' "art.example.com/proxy/${agw#ghcr.io/}" "art.example.com/proxy/${pg#ghcr.io/}" art.example.com/proxy/nvidia/openshell/supervisor:0.1.5 \
	art.example.com/proxy/stateracloud/flavor-opencode:0.0.0 art.example.com/proxy/stateracloud/koinon-agentd:0.0.0 \
	art.example.com/proxy/stateracloud/koinon-api:0.0.0 art.example.com/proxy/stateracloud/koinon-reconciler:0.0.0 | sort -u)
[[ $(image_refs --set global.imageRegistry=art.example.com/proxy) == "$want" ]] ||
	fail "global.imageRegistry did not rewrite every image: $(image_refs --set global.imageRegistry=art.example.com/proxy | tr '\n' ' ')"
# (c) the map wins for its host; imageRegistry rewrites the others (the CNPG image is on another upstream here).
mixed=(--set global.imageRegistry=art.example.com/proxy --set 'global.registryMirrors.ghcr\.io=art.example.com/ghcr-remote'
	--set postgres.cnpg.imageName=quay.io/enterprisedb/postgresql:18@sha256:f7e00138159bb45d70b624ba25e446924cb3ed1bf366d13e936c3f81b914064e)
refs=$(image_refs "${mixed[@]}")
grep -Fxq -- "art.example.com/ghcr-remote/stateracloud/koinon-api:0.0.0" <<<"$refs" || fail "registryMirrors[ghcr.io] must rewrite Koinon's images: $refs"
grep -Fxq -- "art.example.com/ghcr-remote/${agw#ghcr.io/}" <<<"$refs" || fail "registryMirrors[ghcr.io] must rewrite agentgateway, digest kept: $refs"
grep -Fxq -- "art.example.com/proxy/enterprisedb/postgresql:18@sha256:f7e00138159bb45d70b624ba25e446924cb3ed1bf366d13e936c3f81b914064e" <<<"$refs" || fail "imageRegistry must rewrite hosts the map lacks: $refs"
grep -Fq -- "ghcr.io/" <<<"$refs" && fail "a ghcr.io reference survived the mirror: $refs"
# docker.io: no host, and a bare name is docker.io/library/name.
has "bare docker.io names go through the map" 'imageName: art.example.com/docker-remote/library/postgres:18$' \
	--set 'global.registryMirrors.docker\.io=art.example.com/docker-remote' --set postgres.cnpg.imageName=postgres:18
has "user/name is on docker.io" 'imageName: art.example.com/docker-remote/cnpg/postgres:18$' \
	--set 'global.registryMirrors.docker\.io=art.example.com/docker-remote' --set postgres.cnpg.imageName=cnpg/postgres:18
has "a registry with a port is a host" 'imageName: art.example.com/p/img:1$' \
	--set global.imageRegistry=art.example.com/p --set postgres.cnpg.imageName=registry.local:5000/img:1
# A reference already under the mirror is not prefixed again, whichever setting names the mirror.
has "a reference under another mirror value stays" 'imageName: art.example.com/ghcr-remote/org/x:1$' --set global.imageRegistry=proxy.example.com/p \
	--set 'global.registryMirrors.ghcr\.io=art.example.com/ghcr-remote' --set postgres.cnpg.imageName=art.example.com/ghcr-remote/org/x:1
has "image.registry already under imageRegistry does not double" 'image: art.example.com/ghcr-remote/stateracloud/koinon-api:0.0.0$' \
	--set image.registry=art.example.com/ghcr-remote/stateracloud --set global.imageRegistry=art.example.com/ghcr-remote
# Docker Hub's other names and upper case are docker.io.
for h in index.docker.io registry-1.docker.io Docker.IO; do
	has "$h is docker.io" 'imageName: art.example.com/docker-remote/library/postgres:18$' \
		--set 'global.registryMirrors.docker\.io=art.example.com/docker-remote' --set "postgres.cnpg.imageName=$h/library/postgres:18"
done
# A URL is not a registry.
if render --set global.imageRegistry=https://art.example.com >/dev/null 2>&1; then fail "imageRegistry with a scheme must not render"; fi
if render --set 'global.registryMirrors.ghcr\.io=https://art.example.com' >/dev/null 2>&1; then fail "a mirror with a scheme must not render"; fi
# NOTES warn when image.registry is customised next to a global mirror (helm install prints them; template does not).
notes=$("$HELM" install koinon "$CHART" -n koinon --dry-run=client --set openshell.insecure=true --set image.registry=quay.io/m --set global.imageRegistry=art.example.com 2>&1) || true
grep -q 'WARNING: image.registry is quay.io/m' <<<"$notes" || fail "no NOTES warning for image.registry with a global mirror"
# An explicit image.registry is the upstream that the rewrite starts from.
has "image.registry is rewritten too" 'image: art.example.com/ghcr-remote/mine/koinon-api:0.0.0$' \
	--set image.registry=quay.io/mine --set 'global.registryMirrors.quay\.io=art.example.com/ghcr-remote'
has "an explicit image.registry stands without a mirror" 'image: quay.io/mine/koinon-api:0.0.0$' --set image.registry=quay.io/mine
# (e) the default supervisor pin follows the mirror, so the admission policy admits the rewritten image; an explicit list is used as written.
has "supervisor pin is rewritten" 'c.image in \["art.example.com/ghcr-remote/nvidia/openshell/supervisor:0.1.5"\]' \
	--set 'global.registryMirrors.ghcr\.io=art.example.com/ghcr-remote'
has "explicit supervisorImages are not rewritten" 'c.image in \["ghcr.io/nvidia/openshell/supervisor:0.1.6"\]' \
	--set global.imageRegistry=art.example.com/proxy --set 'sandboxPolicy.supervisorImages={ghcr.io/nvidia/openshell/supervisor:0.1.6}'
has "preflight is told the supervisor pin" '--supervisor-images=art.example.com/proxy/nvidia/openshell/supervisor:0.1.5' --set global.imageRegistry=art.example.com/proxy
lacks "no supervisor pin to check when the policy is off" '--supervisor-images' --set sandboxPolicy.enabled=false
# (d) pull secrets: strings and {name} maps, merged with the deprecated top-level list, on every pod-bearing object.
pull_args=(--set 'global.imagePullSecrets[0]=regcred' --set 'global.imagePullSecrets[1].name=other' --set 'imagePullSecrets[0].name=regcred')
rendered=$(render "${pull_args[@]}")
pods=$(grep -cE '^ *(containers|initContainers):' <<<"$rendered" || true)
withsecrets=$(grep -cE '^ +imagePullSecrets:$' <<<"$rendered" || true)
# Deployments (api, reconciler, agentd, gateway), the migrate, ensure-secrets and preflight Jobs: 7 pod
# specs; the CNPG Cluster has its own imagePullSecrets (spec level).
[[ $withsecrets == 8 ]] || fail "imagePullSecrets on $withsecrets objects, want 8 (7 pod specs and the CNPG Cluster); pod specs: $pods"
[[ $(grep -c -- '- name: regcred$' <<<"$rendered") == 8 ]] || fail "regcred must appear once per object, de-duplicated across the two lists"
has "pull secrets reach the preflight probe pods" '--probe-image-pull-secrets=regcred,other$' "${pull_args[@]}"
has "the deprecated top-level list still works alone" '--probe-image-pull-secrets=old$' --set 'imagePullSecrets[0].name=old'
lacks "no imagePullSecrets without any" 'imagePullSecrets:'
has "the preflight expects sandbox pull secrets with a pull secret" '--expect-sandbox-pull-secrets' --set 'global.imagePullSecrets[0]=x'
has "the preflight expects sandbox pull secrets with a mirror" '--expect-sandbox-pull-secrets' --set global.imageRegistry=art.example.com
lacks "the preflight expects none by default" '--expect-sandbox-pull-secrets'
has "the preflight may read OpenShell's gateway ConfigMap, by name" 'resourceNames: \[openshell-config\]'

# Latency defaults (plan item LT1): the reconciler's warm window and job bound reach its config.
has "reconciler config carries the park warm window" 'parkWarm: 15m' --show-only templates/config.yaml
# ask_user (plan item TK3): the reconciler offers the koinon target, served on the tools listener.
has "reconciler config names the ask_user tool server" 'koinonTools:' --show-only templates/config.yaml
has "the ask_user tool server is on the tools Service" 'svc:[0-9]+/mcp-koinon$' --show-only templates/config.yaml
has "reconciler config carries the park warm window override" 'parkWarm: 30m' --set reconciler.parkWarm=30m --show-only templates/config.yaml
has "reconciler config carries the archive window (AR1)" 'archiveAfter: 168h' --show-only templates/config.yaml
has "reconciler config carries the archive window override" 'archiveAfter: 24h' --set reconciler.archiveAfter=24h --show-only templates/config.yaml
has "reconciler config can turn the archive tier off" 'archiveAfter: -1h' --set reconciler.archiveAfter=-1h --show-only templates/config.yaml
has "reconciler config carries the sandbox job bound" 'sandboxJobs: 8' --show-only templates/config.yaml
# Flavor pre-pull (practice B6): off by default; needs a node selector; one init container per
# flavor image through koinon.imageRef and the pull secrets; never in a sandbox namespace.
prepull=(--set flavorPrepull.enabled=true --set-string 'flavorPrepull.nodeSelector.katacontainers\.io/kata-runtime=true')
lacks "no flavor pre-pull by default" 'kind: DaemonSet'
has "flavor pre-pull renders a DaemonSet" '^kind: DaemonSet$' "${prepull[@]}" --show-only templates/flavor-prepull.yaml
has "flavor pre-pull selects the Kata nodes" 'katacontainers.io/kata-runtime: "true"' "${prepull[@]}" --show-only templates/flavor-prepull.yaml
has "flavor pre-pull pulls the flavor image" 'image: "ghcr.io/stateracloud/flavor-opencode:' "${prepull[@]}" --show-only templates/flavor-prepull.yaml
has "flavor pre-pull image follows the registry mirror" 'image: "mirror.example.com/stateracloud/flavor-opencode:' "${prepull[@]}" \
	--set global.imageRegistry=mirror.example.com --show-only templates/flavor-prepull.yaml
has "flavor pre-pull pause image follows the registry mirror" 'image: "mirror.example.com/pause:3.10"' "${prepull[@]}" \
	--set global.imageRegistry=mirror.example.com --show-only templates/flavor-prepull.yaml
has "flavor pre-pull uses the image pull secrets" '- name: regcred' "${prepull[@]}" --set 'global.imagePullSecrets[0].name=regcred' \
	--show-only templates/flavor-prepull.yaml
lacks "flavor pre-pull sets no runtime class (it is not a sandbox)" 'runtimeClassName' "${prepull[@]}" --show-only templates/flavor-prepull.yaml
lacks "flavor pre-pull is not privileged" 'privileged: true' "${prepull[@]}" --show-only templates/flavor-prepull.yaml
has "flavor pre-pull drops all capabilities" 'drop: \[ALL\]' "${prepull[@]}" --show-only templates/flavor-prepull.yaml
if render --set flavorPrepull.enabled=true >/dev/null 2>&1; then fail "flavorPrepull without a nodeSelector must not render"; fi

if [ "$failures" -gt 0 ]; then
	echo "$failures assertion(s) failed" >&2
	exit 1
fi

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

# Home checkpoints (plan item HC1): off by default; on, koinon-agentd (the only holder of the S3
# credentials) gets the bucket, the Secret's keys and a scratch volume (no KEK), and the reconciler
# is told. Nothing renders for the S3 credentials in any other component.
ckpt=(--set checkpoints.enabled=true --set checkpoints.s3.existingSecret=koinon-homes-s3 --set checkpoints.s3.bucket=koinon-homes)
lacks "no checkpoint flags by default" 'KOINON_CHECKPOINTS|AWS_ACCESS_KEY_ID'
lacks "the reconciler is not told to checkpoint by default" 'checkpoints: true'
env_is KOINON_CHECKPOINTS '"true"' "${ckpt[@]}"
env_is KOINON_CHECKPOINTS_S3_BUCKET '"koinon-homes"' "${ckpt[@]}"
env_is KOINON_CHECKPOINTS_INTERVAL '"1h"' "${ckpt[@]}"
has "the reconciler is told to checkpoint" 'checkpoints: true' "${ckpt[@]}" --show-only templates/config.yaml
has "the S3 credentials come from the Secret" 'name: "koinon-homes-s3"' "${ckpt[@]}"
secret_key() { render --show-only templates/deployments.yaml "${ckpt[@]}" | grep -A4 -E -- "- name: $1\$" | grep -Fq -- "key: $2" || fail "$1 reads key $2 of the Secret"; }
secret_key AWS_ACCESS_KEY_ID AWS_ACCESS_KEY_ID
secret_key AWS_SECRET_ACCESS_KEY AWS_SECRET_ACCESS_KEY
secret_key KOINON_CHECKPOINTS_S3_ENDPOINT endpoint
secret_key KOINON_CHECKPOINTS_S3_REGION region
has "agentd gets a scratch volume with the size limit" 'sizeLimit: "9Gi"' "${ckpt[@]}" --set checkpoints.scratchSize=9Gi
# The default holds 2 backups and 1 restore of 4Gi each plus caches (values.yaml).
has "the default scratch size covers the backups, a restore and the caches" 'sizeLimit: "14Gi"' "${ckpt[@]}"
agentd_kekenv=$(render --show-only templates/deployments.yaml "${ckpt[@]}" | awk 'BEGIN{RS="\n---\n"} /name: koinon-agentd|component: agentd/ && /KOINON_KEK_DIR/{n++} END{print n+0}')
[[ $agentd_kekenv == 0 ]] || fail "koinon-agentd must not get KOINON_KEK_DIR (koinon-api hands out the repository passwords, ADR 0054)"
for mode in default checkpoints; do
  args=(); [[ $mode == checkpoints ]] && args=("${ckpt[@]}")
  rec_kek=$(render --show-only templates/deployments.yaml ${args[@]+"${args[@]}"} | awk 'BEGIN{RS="\n---\n"} /name: koinon-reconciler/ && /name: KOINON_KEK_DIR/ && /\/secrets\/kek/{n++} END{print n+0}')
  [[ $rec_kek == 1 ]] || fail "the reconciler must render KOINON_KEK_DIR=/secrets/kek ($mode mode), found $rec_kek"
done
agentd_kek=$(render --show-only templates/deployments.yaml "${ckpt[@]}" | awk 'BEGIN{RS="\n---\n"} /name: koinon-agentd|component: agentd/ && /mountPath: \/secrets\/kek/{n++} END{print n+0}')
[[ $agentd_kek == 0 ]] || fail "koinon-agentd must not mount the KEKs with checkpoints on (ADR 0054)"
holders=$(render --show-only templates/deployments.yaml "${ckpt[@]}" | awk 'BEGIN{RS="\n---\n"} /AWS_SECRET_ACCESS_KEY/{n++} END{print n+0}')
[[ $holders == 1 ]] || fail "the S3 secret key is in exactly one Deployment (agentd), found $holders"
if render --set checkpoints.enabled=true --set checkpoints.s3.bucket=b >/dev/null 2>&1; then fail "checkpoints without s3.existingSecret must not render"; fi
if render --set checkpoints.enabled=true --set checkpoints.s3.existingSecret=s >/dev/null 2>&1; then fail "checkpoints without s3.bucket must not render"; fi
if render "${ckpt[@]}" --set checkpoints.s3.pathStyle=false >/dev/null 2>&1; then fail "checkpoints.s3.pathStyle=false must not render"; fi
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
