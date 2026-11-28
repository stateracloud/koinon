# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: koinon-gateway, the MCP path (ADR 0097 step 2, issue #278).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

mcp=(--set gateway.mcp.driver=koinon)
fb=(--set gateway.mcp.driver=agentgateway)
mcp_only=(--show-only templates/gateway-koinon-mcp.yaml)

# Default (ADR 0097 decision 5, #1239): koinon-gateway serves every MCP path. koinon-api sends approved MCP calls
# and the live check of an agent's MCP tools there, the agents' harness reaches it, and the LLM path stays on
# agentgateway.
has "koinon-gateway pods render by default" 'component: mcp-gateway' "${mcp_only[@]}"
for driver in default koinon; do
  args=()
  [ "$driver" = default ] || args=("${mcp[@]}")
  env_is KOINON_GATEWAY_MCP_URL '"http://koinon-mcp-gateway.koinon.svc.cluster.local:8080/mcp"' "${args[@]}"
  env_is KOINON_GATEWAY_MCP_CHECK_URL '"http://koinon-mcp-gateway.koinon.svc.cluster.local:8080/mcp"' "${args[@]}"
  env_is KOINON_AGENT_MCP_URL '"http://koinon-mcp-gateway.koinon.svc.cluster.local:8080/mcp"' "${args[@]}"
  env_is KOINON_GATEWAY_URL '"http://koinon-gateway.koinon.svc.cluster.local:8080"' "${args[@]}"
  has "the reconciler is told koinon-gateway's MCP listener ($driver)" 'mcpUrl: http://koinon-mcp-gateway.koinon.svc.cluster.local:8080$' "${args[@]}" --show-only templates/config.yaml
done
has "agentgateway still renders as the LLM path" 'ghcr.io/agentgateway/agentgateway'

# The fallback (gateway.mcp.driver=agentgateway): agentgateway serves MCP as before, and nothing of koinon-gateway
# renders. agentd sets no MCP URL, so the harness takes <gateway URL>/mcp.
lacks "no koinon-gateway pods with the fallback" 'component: mcp-gateway' "${fb[@]}"
env_is KOINON_GATEWAY_MCP_URL '"http://koinon-gateway.koinon.svc.cluster.local:8080/mcp"' "${fb[@]}"
env_is KOINON_GATEWAY_MCP_CHECK_URL '"http://koinon-gateway.koinon.svc.cluster.local:8080/mcp"' "${fb[@]}"
lacks "agentd sets no MCP URL with the fallback" 'KOINON_AGENT_MCP_URL' "${fb[@]}" --show-only templates/deployments.yaml
lacks "the reconciler is told no MCP listener with the fallback" 'mcpUrl' "${fb[@]}" --show-only templates/config.yaml

# Two replicas, rolling without a gap, spread over nodes.
has "koinon-gateway runs two replicas" '^  replicas: 2$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway rollout keeps every pod until the new one is ready" 'maxUnavailable: 0$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway pods spread over nodes" 'topologyKey: kubernetes.io/hostname$' "${mcp[@]}" "${mcp_only[@]}"
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" "${mcp[@]}" --set gateway.mcp.koinon.replicas=1 >/dev/null 2>&1; then
  fail "koinon-gateway accepts one replica"
fi

# The metrics (15020) and health (15021) listeners are fixed, so the serving port cannot take them.
for p in 15020 15021; do
  if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" "${mcp[@]}" --set gateway.mcp.koinon.port=$p >/dev/null 2>&1; then
    fail "koinon-gateway accepts port $p, the fixed metrics or health port"
  fi
done

# Drain: the pod grace period is the shutdown delay, the drain timeout and 30 seconds, and the process gets the timeout.
has "koinon-gateway grace period is the drain timeout plus 30" 'terminationGracePeriodSeconds: 305$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway is told the drain timeout" '--drain-timeout=270s$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway is told the shutdown delay" '--shutdown-delay=5s$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway grace period follows the drain timeout" 'terminationGracePeriodSeconds: 335$' "${mcp[@]}" --set gateway.mcp.koinon.drainTimeoutSeconds=300 "${mcp_only[@]}"

# A drain shorter than twice the call timeout (an approval wait and an upstream call, 120 s each) would cut each call in flight at a rollout (issue 1347).
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" "${mcp[@]}" --set gateway.mcp.koinon.drainTimeoutSeconds=200 >/dev/null 2>&1; then
  fail "koinon-gateway accepts a drain timeout shorter than twice its call timeout"
fi
has "koinon-gateway accepts a drain timeout equal to twice its call timeout" '--drain-timeout=240s$' "${mcp[@]}" --set gateway.mcp.koinon.drainTimeoutSeconds=240 "${mcp_only[@]}"
has "koinon-gateway readiness is /readyz" 'path: /readyz,' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway liveness is /healthz" 'path: /healthz,' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway has a PodDisruptionBudget" '^kind: PodDisruptionBudget$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway PodDisruptionBudget takes one pod at a time" '^  maxUnavailable: 1$' "${mcp[@]}" "${mcp_only[@]}"

# The image and pull secrets follow the chart's rules, like every Koinon binary.
has "koinon-gateway image is built from image.registry" 'image: ghcr.io/stateracloud/koinon/gateway:' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway image follows a registry mirror" 'image: mirror.example.test/stateracloud/koinon/gateway:' "${mcp[@]}" --set 'global.registryMirrors.ghcr\.io=mirror.example.test' "${mcp_only[@]}"
has "koinon-gateway uses the image pull secrets" 'name: pull-secret$' "${mcp[@]}" --set 'global.imagePullSecrets[0]=pull-secret' "${mcp_only[@]}"

# Secret custody (ADR 0097 decision 5, ADR 0120): no key-encryption key and no Kubernetes API token. The only
# database access is the guard's own login. The pod mounts the MCP credentials Secret and the OTLP token, and
# no LLM provider key.
lacks "koinon-gateway mounts no KEK" 'kek|KEK' "${mcp[@]}" "${mcp_only[@]}"
lacks "koinon-gateway mounts no provider keys" 'koinon-gateway-providers|/secrets/providers' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway mounts the MCP credentials Secret" 'secretName: koinon-gateway-mcp-keys$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway mounts the MCP credentials read-only" 'mountPath: /secrets/mcp$' "${mcp[@]}" "${mcp_only[@]}"
n=$(render "${mcp[@]}" "${mcp_only[@]}" | grep -c 'automountServiceAccountToken: false' || true)
[ "$n" = 2 ] || fail "koinon-gateway mounts a service account token ($n of 2 places say false)"
# agentgateway mounts both Secrets: it serves MCP with the fallback, and with either driver its config keeps the
# MCP targets whose credential it can open (the reconciler renders one State for both gateways).
has "agentgateway mounts the provider keys" 'secretName: koinon-gateway-providers$' --show-only templates/gateway-standalone.yaml
has "agentgateway mounts the MCP credentials" 'secretName: koinon-gateway-mcp-keys$' --show-only templates/gateway-standalone.yaml
has "agentgateway mounts the MCP credentials with the fallback" 'secretName: koinon-gateway-mcp-keys$' "${fb[@]}" --show-only templates/gateway-standalone.yaml
has "the reconciler is told the MCP credentials Secret" 'mcpKeysSecret: koinon-gateway-mcp-keys$' --show-only templates/config.yaml
has "the reconciler is told the MCP credentials directory" 'mcpKeysDir: /secrets/mcp$' --show-only templates/config.yaml
lacks "the reconciler serves no sealed credential with the fallback" 'mcpSealed' "${fb[@]}" --show-only templates/config.yaml
has "the reconciler serves sealed credentials by default" 'mcpSealed: true$' --show-only templates/config.yaml
# The database login (cnpg mode): its own role, a member of koinon_gateway only, with a Secret of its own.
has "koinon-gateway database role koinon_mcpgateway" 'name: koinon_mcpgateway$' "${mcp[@]}" --show-only templates/postgres-cnpg.yaml
has "koinon-gateway database role is a member of koinon_gateway only" 'inRoles: \[koinon_gateway\]$' "${mcp[@]}" --show-only templates/postgres-cnpg.yaml
has "the group role koinon_gateway is created" 'CREATE ROLE koinon_gateway NOLOGIN$' --show-only templates/postgres-cnpg.yaml
lacks "no koinon_mcpgateway role with the fallback" 'koinon_mcpgateway' "${fb[@]}" --show-only templates/postgres-cnpg.yaml
has "the database login has a generated Secret" 'koinon-db-mcpgateway' "${mcp[@]}" --show-only templates/ensure-secrets.yaml
lacks "no database login Secret with the fallback" 'koinon-db-mcpgateway' "${fb[@]}" --show-only templates/ensure-secrets.yaml
has "koinon-gateway reads the database URL from a file" '--database-url-file=/etc/koinon/mcp-db/url$' "${mcp[@]}" "${mcp_only[@]}"
has "the database URL has no password" 'url: "postgres://koinon_mcpgateway@koinon-pg-rw.koinon.svc:5432/koinon\?sslmode=verify-full&sslrootcert=/etc/koinon/db-ca/ca.crt"$' "${mcp[@]}" "${mcp_only[@]}"
has "the password comes from the login Secret" 'name: koinon-db-mcpgateway$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway trusts the database CA" 'mountPath: /etc/koinon/db-ca$' "${mcp[@]}" "${mcp_only[@]}"
has "a supplied login Secret is used" 'name: mine-mcpgateway$' "${mcp[@]}" --set postgres.cnpg.existingSecrets.mcpgateway=mine-mcpgateway "${mcp_only[@]}"
# External mode: the URL is a key of the external Secret, and the key can be renamed.
ext=(--set postgres.mode=external --set postgres.external.secretName=koinon-db)
has "external mode mounts the login URI as the URL file" 'key: mcpgateway-uri$' "${mcp[@]}" "${ext[@]}" "${mcp_only[@]}"
has "external mode names the key by postgres.external.keys.mcpgateway" 'key: my-gw-uri$' "${mcp[@]}" "${ext[@]}" --set postgres.external.keys.mcpgateway=my-gw-uri "${mcp_only[@]}"
lacks "external mode has no PGPASSWORD" 'PGPASSWORD' "${mcp[@]}" "${ext[@]}" "${mcp_only[@]}"
lacks "external mode renders no URL ConfigMap" 'kind: ConfigMap' "${mcp[@]}" "${ext[@]}" "${mcp_only[@]}"
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" "${mcp[@]}" "${ext[@]}" --set postgres.external.keys.mcpgateway= >/dev/null 2>&1; then
  fail "external mode renders with an empty postgres.external.keys.mcpgateway"
fi
has "koinon-gateway reads the State stream token from the OTLP token Secret" '--state-token-file=/secrets/otlp/token$' "${mcp[@]}" "${mcp_only[@]}"
lacks "koinon-gateway has no RBAC" 'name: koinon-mcp-gateway$' "${mcp[@]}" --show-only templates/rbac.yaml
has "koinon-gateway runs non-root with a read-only root" 'readOnlyRootFilesystem: true$' "${mcp[@]}" "${mcp_only[@]}"

# Network: koinon-api's State stream port admits the pods, and the pods have a NetworkPolicy of their own.
# netpol_doc NAME [helm args]: the one NetworkPolicy document called NAME, so an assertion cannot
# pass on text that another policy renders.
netpol_doc() {
  local n=$1; shift
  render "$@" --show-only templates/networkpolicy.yaml | awk -v n="$n" 'BEGIN { RS = "\n---\n" } $0 ~ ("\n  name: " n "\n") { print }'
}
api_np=$(netpol_doc koinon-api "${mcp[@]}")
grep -q 'component: mcp-gateway$' <<<"$api_np" || fail "the State stream port of koinon-api does not admit koinon-gateway"
api_off=$(netpol_doc koinon-api "${fb[@]}")
if grep -q 'component: mcp-gateway$' <<<"$api_off"; then fail "the State stream port admits koinon-gateway when it is not selected"; fi
[ -z "$(netpol_doc koinon-mcp-gateway "${fb[@]}")" ] || fail "koinon-gateway has a NetworkPolicy with the fallback"
mcp_np=$(netpol_doc koinon-mcp-gateway "${mcp[@]}")
[ -n "$mcp_np" ] || fail "koinon-gateway has no NetworkPolicy of its own"
grep -q '10.0.0.0/8' <<<"$mcp_np" || fail "koinon-gateway egress does not carve out the private ranges"
# The agents reach the MCP port as they reach agentgateway's: from the sandbox namespaces (OpenShell), or from
# Substrate's namespace through atenet. Koinon's own pods (koinon-api) reach it too.
mcp_ing=$(awk '/^  ingress:/{f=1} f' <<<"$mcp_np")
grep -q 'openshell.ai/managed-by' <<<"$mcp_ing" || fail "koinon-gateway does not admit the sandbox namespaces"
grep -q 'app.kubernetes.io/instance: koinon$' <<<"$mcp_ing" || fail "koinon-gateway does not admit Koinon's own pods"
if grep -q 'ipBlock' <<<"$mcp_ing"; then fail "koinon-gateway ingress admits an ipBlock"; fi
sub_ing=$(awk '/^  ingress:/{f=1} f' <<<"$(netpol_doc koinon-mcp-gateway --set execution.driver=substrate \
  --set gateway.model.driver=koinon --set gateway.mcp.driver=koinon --set execution.substrate.controlAddress=api.ate-system.svc:443 --set execution.substrate.routerAddress=atenet-router.ate-system.svc:443 \
  --set execution.substrate.tokenAudience=ateapi --set execution.substrate.allowUnhardened=true --set execution.substrate.defaultTier=dev \
  --set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --set sandboxLimitRange.enabled=false)")
grep -q 'kubernetes.io/metadata.name: "ate-system"$' <<<"$sub_ing" || fail "koinon-gateway does not admit Substrate's namespace (execution.driver=substrate)"
# Issue #1489: on Substrate the MCP port admits only atenet-egress, which adds the actor JWT, not the whole namespace.
grep -q 'app: atenet-egress$' <<<"$sub_ing" || fail "koinon-gateway's MCP port does not admit atenet-egress on Substrate"
if grep -q 'kubernetes.io/metadata.name: ate-system$' <<<"$sub_ing"; then fail "koinon-gateway's MCP port admits all of Substrate's namespace (issue #1489)"; fi
# Substrate: the control plane's egress reaches koinon-gateway's MCP pods by default, and not with the fallback.
sub_np=$(render_default -f "$CHART/ci/substrate-values.yaml" --show-only templates/substrate-networkpolicy.yaml)
grep -q 'component: mcp-gateway$' <<<"$sub_np" || fail "the Substrate control plane egress does not reach koinon-gateway"
# Issue #1489: Substrate refuses the fallback, because only koinon-gateway checks the actor JWT.
if render_default -f "$CHART/ci/substrate-values.yaml" "${fb[@]}" >/dev/null 2>&1; then fail "Substrate renders with the agentgateway MCP fallback"; fi
# Issue #1372: the control plane reaches koinon-gateway's MCP pods on the named ports an actor needs, not on every port.
# The stats port (the metrics of all agents) and the health port stay closed.
# mcp_rules <rendered policy>: each egress rule that has a koinon-gateway peer, each ended by a line "@@@". A rule ends at the
# next rule, at the end of the egress list or at a document boundary.
mcp_rules() { awk '
function flush() { if (buf ~ /component: mcp-gateway/) printf "%s@@@\n", buf; buf = "" }
/^---$/ { flush(); f = 0; next }
/^  egress:/ { flush(); f = 1; next }
f && /^    - / { flush(); buf = $0 "\n"; next }
f && /^  [a-z]/ { flush(); f = 0; next }
f { buf = buf $0 "\n" }
END { flush() }' <<<"$1"; }
# mcp_rule_check <label> <rendered policy> <yes|no: egress listener on>: exactly one rule reaches koinon-gateway, and it lists
# only the named ports of an actor.
mcp_rule_check() {
	local rules n
	rules=$(mcp_rules "$2")
	n=$(grep -c '^@@@$' <<<"$rules" || true)
	[ "$n" = 1 ] || { fail "$1: $n egress rules reach koinon-gateway, want exactly one (a second rule can open every port)"; return; }
	grep -q 'ports:' <<<"$rules" || fail "$1: the egress rule for koinon-gateway has no ports: every port would be open"
	grep -q 'port: mcp,' <<<"$rules" || fail "$1: the egress rule for koinon-gateway does not name the mcp port"
	if grep -Eq 'port: (stats|health|15020|15021)' <<<"$rules"; then fail "$1: the egress rule for koinon-gateway opens the stats or health port"; fi
	if [ "$3" = yes ]; then
		grep -q 'port: egress,' <<<"$rules" || fail "$1: the egress rule for koinon-gateway does not name the egress port with the egress listener"
	elif grep -q 'port: egress,' <<<"$rules"; then
		fail "$1: the egress rule for koinon-gateway names the egress port without the egress listener"
	fi
}
mcp_rule_check "Substrate control plane" "$sub_np" no
mcp_rule_check "Substrate control plane with the egress listener" "$(render_default -f "$CHART/ci/substrate-values.yaml" --set gateway.mcp.koinon.egress.enabled=true --show-only templates/substrate-networkpolicy.yaml)" yes
grep -q 'port: 8081$' <<<"$mcp_np" || fail "koinon-gateway egress does not reach the tools port of koinon-api"
grep -q 'cnpg.io/cluster: koinon-pg$' <<<"$mcp_np" || fail "koinon-gateway egress does not reach the Postgres pods"
grep -q 'port: 5432$' <<<"$mcp_np" || fail "koinon-gateway egress does not name the Postgres port"
if grep -q 'port: 9443\|port: guard' <<<"$mcp_np"; then fail "koinon-gateway egress names a guard port: the guard runs in the process"; fi
ext_np=$(netpol_doc koinon-mcp-gateway "${mcp[@]}" "${ext[@]}")
if grep -q 'cnpg.io/cluster' <<<"$ext_np"; then fail "koinon-gateway egress names Postgres pods in external mode"; fi
has "gatewayEgressTo reaches an external database" '10.1.2.3/32' "${mcp[@]}" "${ext[@]}" --set 'networkPolicy.gatewayEgressTo[0].to[0].ipBlock.cidr=10.1.2.3/32' --show-only templates/networkpolicy.yaml
tools_ing=$(awk '/- port: tools/{f=1} f&&/- ports:|^---/{if(NR>1&&c++>0)exit} f{print}' <<<"$api_np")
grep -q 'component: mcp-gateway$' <<<"$tools_ing" || fail "the tools port of koinon-api does not admit koinon-gateway"
if grep -q 'component: mcp-gateway$' <<<"$(awk '/- port: guard/{f=1} f&&n++<12' <<<"$api_np")"; then fail "the guard port of koinon-api admits koinon-gateway"; fi

# The post-upgrade grants hook waits for the gateway role when the driver is on, and only then.
has "the grants hook waits for the gateway role" '"--wait-gateway-role"' "${mcp[@]}" --set postgres.mode=cnpg --show-only templates/install-audit-grants.yaml
lacks "the grants hook does not wait for the gateway role with the fallback" 'wait-gateway-role' "${fb[@]}" --set postgres.mode=cnpg --show-only templates/install-audit-grants.yaml

# The koinon driver needs standalone mode: kubernetes mode is frozen. Kubernetes mode renders with the fallback.
for args in "" "${mcp[*]}"; do
  # shellcheck disable=SC2086
  if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" $args --set gateway.mode=kubernetes >/dev/null 2>&1; then
    fail "gateway.mcp.driver=koinon renders with gateway.mode=kubernetes ('$args')"
  fi
done
render "${fb[@]}" --set gateway.mode=kubernetes >/dev/null || fail "gateway.mode=kubernetes does not render with gateway.mcp.driver=agentgateway"
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" --set gateway.mcp.driver=other >/dev/null 2>&1; then
  fail "gateway.mcp.driver accepts an unknown driver"
fi
