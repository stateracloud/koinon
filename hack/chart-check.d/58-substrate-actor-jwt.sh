# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the actor token of the egress listener on Substrate (ADR 0120, amendment of issue #1386, items 4,
# 7 and 8). On substrate the gateway verifies the token that atenet-egress adds against Substrate's issuer; there is
# no Ed25519 key, and only atenet-egress reaches the egress port. On openshell nothing changes (57-gateway-egress.sh).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers and wsub and docof of 44-substrate-worker.sh.

aj=("${wsub[@]}" --set gateway.mcp.driver=koinon --set gateway.mcp.koinon.egress.enabled=true)
aj_os=(--set gateway.mcp.driver=koinon --set gateway.mcp.koinon.egress.enabled=true)
aj_gw=(--show-only templates/gateway-koinon-mcp.yaml)
aj_np=(--show-only templates/networkpolicy.yaml)
# The full render goes to kubeconform and chartlint with the ci values files: no ci values file, because the chart
# smoke test would start this gateway, and it cannot reach an issuer there.
render "${aj[@]}" >"$out/substrate-actor-jwt.yaml"

# The gateway: the Substrate verifier, one issuer from the values, the CA file.
has "substrate: the gateway runs the Substrate verifier" '^            - --actor-token-verifier=substrate$' "${aj[@]}" "${aj_gw[@]}"
has "substrate: the issuer is Substrate's installer default" '^            - "--actor-jwt-issuer=https://idp.ate-system.svc"$' "${aj[@]}" "${aj_gw[@]}"
has "substrate: the gateway reads the issuer CA file" '^            - --actor-jwt-ca-file=/etc/koinon/substrate-idp-ca/ca.crt$' "${aj[@]}" "${aj_gw[@]}"
has "substrate: the default issuer follows the Substrate namespace" '"--actor-jwt-issuer=https://idp.team-a.svc"$' "${aj[@]}" \
	--set execution.substrate.namespace=team-a --set 'execution.substrate.gatewayClientNamespaceSelector.matchLabels.kubernetes\.io/metadata\.name=team-a' "${aj_gw[@]}"
has "substrate: a set issuer is used as given" '"--actor-jwt-issuer=https://idp.example.test"$' "${aj[@]}" \
	--set execution.substrate.actorJwt.issuer=https://idp.example.test "${aj_gw[@]}"
for bad in http://idp.example.test https://idp.example.test/prod 'https://idp.example.test?x=1'; do
	if render "${aj[@]}" --set "execution.substrate.actorJwt.issuer=$bad" "${aj_gw[@]}" >/dev/null 2>&1; then
		fail "substrate: the issuer $bad must not render (https://<host> only)"
	fi
done

# No Ed25519 path on substrate: no flag, no key mount, no key Secret, no seed in agentd.
lacks "substrate: no Ed25519 issuer flag" '--actor-token-issuer' "${aj[@]}" "${aj_gw[@]}"
lacks "substrate: no Ed25519 key directory" '--actor-token-keys-dir|actor-token-keys' "${aj[@]}" "${aj_gw[@]}"
lacks "substrate: no actor token key Secret anywhere" 'actor-token-key|/secrets/actor-token' "${aj[@]}"
lacks "substrate: agentd gets no signing setting" 'KOINON_ACTOR_TOKEN_' "${aj[@]}" --show-only templates/deployments.yaml
env_is KOINON_EGRESS_URL '"http://koinon-mcp-gateway.koinon.svc.cluster.local:8081"' "${aj[@]}"
n=$(render "${aj[@]}" --show-only templates/ensure-secrets.yaml | grep -c 'actor-token' || true)
[ "$n" = 0 ] || fail "substrate: ensure-secrets names the actor token key $n times, not 0"

# The CA: a read-only mount of the servicedns ClusterTrustBundle, or a ConfigMap that replaces it. Never a Secret.
gw=$(render "${aj[@]}" "${aj_gw[@]}")
grep -Eq -A2 '^            - name: substrate-idp-ca$' <<<"$gw" || fail "substrate: the gateway does not mount the issuer CA"
grep -A2 '^            - name: substrate-idp-ca$' <<<"$gw" | hit -F 'readOnly: true' || fail "substrate: the issuer CA mount must be read-only"
vol=$(awk '/^        - name: substrate-idp-ca$/{p=1; print; next} p && /^        - name: /{p=0} p' <<<"$gw")
grep -Fq 'signerName: "servicedns.podcert.ate.dev/identity"' <<<"$vol" || fail "substrate: the CA must be the servicedns ClusterTrustBundle"
grep -Fq 'podcert.ate.dev/canarying: live' <<<"$vol" || fail "substrate: the CA bundle must select the live bundles"
grep -Fq 'path: ca.crt' <<<"$vol" || fail "substrate: the CA bundle must be the file ca.crt"
if grep -Eq 'secret:|secretName' <<<"$vol"; then fail "substrate: the issuer CA volume must hold no Secret"; fi
cm=$(render "${aj[@]}" --set execution.substrate.actorJwt.ca.configMap=idp-ca "${aj_gw[@]}" |
	awk '/^        - name: substrate-idp-ca$/{p=1; print; next} p && /^        - name: /{p=0} p')
grep -Fq 'name: "idp-ca"' <<<"$cm" || fail "substrate: a set CA ConfigMap must be mounted"
if grep -Fq 'clusterTrustBundle' <<<"$cm"; then fail "substrate: a set CA ConfigMap must replace the ClusterTrustBundle"; fi

# Network: the gateway reaches the ate-idp-server pods on their pod port only; the egress port admits only atenet-egress.
pol=$(render "${aj[@]}" "${aj_np[@]}" | docof 'name: koinon-mcp-gateway\n')
egr=$(awk '/^  egress:$/{p=1; next} /^  ingress:$/{p=0} p' <<<"$pol")
idp=$(awk 'BEGIN{RS="\n    - "} /app: ate-idp-server/' <<<"$egr")
[ -n "$idp" ] || fail "substrate: the gateway has no egress rule to ate-idp-server"
grep -Fq 'kubernetes.io/metadata.name: "ate-system"' <<<"$idp" || fail "substrate: the issuer rule must name the Substrate namespace"
grep -Fq '{port: 8443, protocol: TCP}' <<<"$idp" || fail "substrate: the issuer rule must open the pod port 8443"
if [ "$(grep -c 'port: ' <<<"$idp")" != 1 ]; then fail "substrate: the issuer rule must open one port only"; fi
if grep -Fq 'port: 443' <<<"$idp"; then fail "substrate: the issuer rule must not use the Service port 443"; fi
ing=$(awk '/^  ingress:$/{p=1; next} p' <<<"$pol" | awk 'BEGIN{RS="\n    - "} /port: egress/')
[ -n "$ing" ] || fail "substrate: the gateway policy has no egress-port rule"
grep -Fq 'app: atenet-egress' <<<"$ing" || fail "substrate: the egress port must admit the atenet-egress pods"
grep -Fq 'kubernetes.io/metadata.name: "ate-system"' <<<"$ing" || fail "substrate: the egress port must name the Substrate namespace"
if [ "$(grep -c 'namespaceSelector:' <<<"$ing")" != 1 ] || [ "$(grep -c 'podSelector:' <<<"$ing")" != 1 ]; then
	fail "substrate: the egress port must have one peer (the atenet-egress pods), not a namespace alone"
fi
if render "${aj[@]}" --set 'execution.substrate.actorJwt.idpPodSelector=null' "${aj_np[@]}" >/dev/null 2>"$out/aj.err"; then
	fail "substrate: an empty ate-idp-server selector must not render"
fi
grep -Fq 'idpPodSelector needs' "$out/aj.err" || fail "substrate: the empty-selector error must name idpPodSelector"
# The worker policy is off here, so the error comes from the gateway policy alone.
if render "${aj[@]}" --set execution.substrate.workerNetworkPolicy.enabled=false \
	--set 'execution.substrate.workerNetworkPolicy.atenetEgressSelector.matchLabels=null' "${aj_np[@]}" >/dev/null 2>"$out/aj.err"; then
	fail "substrate: an empty atenet-egress selector must not render"
fi
grep -Fq 'reach the gateway listeners' "$out/aj.err" || fail "substrate: the empty atenet-egress selector error must name the gateway listeners"

# Issue #1489 (ADR 0120 amendment of 2026-11-27): the model and MCP listeners check the actor JWT too, also without
# the egress listener. The model and MCP paths must be koinon-gateway's, agents send model calls there, and only
# atenet-egress reaches the listeners.
for gw_tpl in gateway-koinon-mcp gateway-koinon-model; do
	has "substrate without the listener: $gw_tpl runs the Substrate verifier" '^            - --actor-token-verifier=substrate$' "${wsub[@]}" --show-only "templates/$gw_tpl.yaml"
	has "substrate without the listener: $gw_tpl reads the issuer CA file" '^            - --actor-jwt-ca-file=/etc/koinon/substrate-idp-ca/ca.crt$' "${wsub[@]}" --show-only "templates/$gw_tpl.yaml"
	has "substrate without the listener: $gw_tpl mounts the issuer CA" '^            - name: substrate-idp-ca$' "${wsub[@]}" --show-only "templates/$gw_tpl.yaml"
	lacks "substrate without the listener: $gw_tpl has no Ed25519 setting" '--actor-token-issuer|actor-token-keys' "${wsub[@]}" --show-only "templates/$gw_tpl.yaml"
done
lacks "substrate without the listener: no egress listener flag" '--egress-addr' "${wsub[@]}" "${aj_gw[@]}"
env_is KOINON_GATEWAY_URL '"http://koinon-model-gateway.koinon.svc.cluster.local:8080"' "${wsub[@]}"
has "substrate: the reconciler gets the model listener" '^      modelUrl: http://koinon-model-gateway.koinon.svc.cluster.local:8080$' "${wsub[@]}" --show-only templates/config.yaml
env_is KOINON_GATEWAY_URL '"http://gw.example.test:8080"' "${wsub[@]}" --set agentd.gatewayURL=http://gw.example.test:8080
for drv in gateway.model.driver gateway.mcp.driver; do
	if render "${wsub[@]}" --set "$drv=agentgateway" >/dev/null 2>"$out/aj.err"; then
		fail "substrate: $drv=agentgateway must not render (issue #1489)"
	fi
	grep -Fq "needs $drv=koinon" "$out/aj.err" || fail "substrate: the error must name $drv=koinon"
done
for listener in koinon-mcp-gateway:mcp koinon-model-gateway:model; do
	pol=$(render "${wsub[@]}" "${aj_np[@]}" | docof "name: ${listener%%:*}\n")
	egr=$(awk '/^  egress:$/{p=1; next} /^  ingress:$/{p=0} p' <<<"$pol")
	grep -Fq 'app: ate-idp-server' <<<"$egr" || fail "substrate: ${listener%%:*} has no egress rule to ate-idp-server"
	ing=$(awk '/^  ingress:$/{p=1; next} p' <<<"$pol" | awk -v port="port: ${listener##*:}" 'BEGIN{RS="\n    - "} index($0, port)')
	grep -Fq 'app: atenet-egress' <<<"$ing" || fail "substrate: the ${listener##*:} port must admit the atenet-egress pods"
	if grep -Fq 'kubernetes.io/metadata.name: ate-system' <<<"$ing"; then
		fail "substrate: the ${listener##*:} port must not admit all of Substrate's namespace"
	fi
	n=$(grep -c 'namespaceSelector:' <<<"$ing" || true)
	[ "$n" = 1 ] || fail "substrate: the ${listener##*:} port has $n namespace peers, want 1 (the atenet-egress pods)"
done

# openshell: the Ed25519 path as before, and none of the Substrate settings.
lacks "openshell: no Substrate verifier" 'actor-token-verifier|actor-jwt|substrate-idp-ca' "${aj_os[@]}" "${aj_gw[@]}"
has "openshell: the Ed25519 key directory stays" '^            - --actor-token-keys-dir=/etc/koinon/actor-token-keys$' "${aj_os[@]}" "${aj_gw[@]}"
lacks "openshell: no issuer route and no atenet-egress fence" 'ate-idp-server|atenet-egress' "${aj_os[@]}" "${aj_np[@]}"
