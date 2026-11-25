# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: koinon-gateway's egress listener and the actor token (ADR 0097, ADR 0120 sections 1
# and 2, issue #1086 part C2). Sourced in order by hack/chart-check.sh; it uses that script's helpers.

eg=(--set gateway.mcp.driver=koinon --set gateway.mcp.koinon.egress.enabled=true)
eg_gw=(--show-only templates/gateway-koinon-mcp.yaml)
eg_agentd=(--show-only templates/deployments.yaml)

# Off by default, and off with the koinon driver alone: no listener, no key, no agentd setting.
lacks "no egress listener by default" '--egress-addr' --set gateway.mcp.driver=koinon "${eg_gw[@]}"
lacks "no actor token key by default" 'actor-token' --set gateway.mcp.driver=koinon
lacks "agentd sends no git to the gateway by default" 'KOINON_EGRESS_URL'
lacks "the reconciler adds no egress sandbox rule by default" 'egressUrl' --set gateway.mcp.driver=koinon --show-only templates/config.yaml

# Only koinon-gateway opens the sealed forge credentials, so the listener needs the koinon driver.
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" \
  --set gateway.mcp.koinon.egress.enabled=true >/dev/null 2>&1; then
  fail "the egress listener renders without gateway.mcp.driver=koinon"
fi
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" "${eg[@]}" \
  --set gateway.mcp.koinon.egress.port=8080 >/dev/null 2>&1; then
  fail "the egress listener takes the MCP listener's port"
fi
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" "${eg[@]}" \
  --set gateway.mcp.koinon.egress.actorToken.keyId=../x >/dev/null 2>&1; then
  fail "the actor token key ID may name a path"
fi

# The gateway: its own listener and Service port, the issuer, the namespace for the dialer, and the public key only.
has "the gateway serves the egress listener" '--egress-addr=:8081$' "${eg[@]}" "${eg_gw[@]}"
has "the gateway checks the actor token issuer" '--actor-token-issuer=koinon-agentd$' "${eg[@]}" "${eg_gw[@]}"
has "the gateway reads the actor token keys" '--actor-token-keys-dir=/etc/koinon/actor-token-keys$' "${eg[@]}" "${eg_gw[@]}"
has "the gateway is told its namespace" 'fieldPath: metadata.namespace$' "${eg[@]}" "${eg_gw[@]}"
has "the egress Service port" 'targetPort: egress$' "${eg[@]}" "${eg_gw[@]}"
has "the gateway mounts the public key as <kid>.pub" 'path: k1.pub$' "${eg[@]}" "${eg_gw[@]}"
has "a new key ID renames the public key file" 'path: k2.pub$' "${eg[@]}" --set gateway.mcp.koinon.egress.actorToken.keyId=k2 "${eg_gw[@]}"
has "the gateway mounts public.pem of the key Secret" 'key: public.pem$' "${eg[@]}" "${eg_gw[@]}"
lacks "the gateway never mounts the seed" 'key: seed' "${eg[@]}" "${eg_gw[@]}"
has "the egress dialer allows the platform's internal hosts" '--egress-allowed-hosts=git.example.test:443$' "${eg[@]}" \
  --set 'egress.allowedInternalHosts={git.example.test:443}' "${eg_gw[@]}"
lacks "no CA file without a bundle" '--egress-ca-file' "${eg[@]}" "${eg_gw[@]}"
has "a CA bundle is a file for the egress upstreams" '--egress-ca-file=/etc/koinon/egress-ca/ca.crt$' "${eg[@]}" \
  --set-string gateway.mcp.koinon.egress.caBundle=koinon-test-not-a-real-ca "${eg_gw[@]}"
has "the CA bundle is a ConfigMap" 'name: koinon-mcp-gateway-egress-ca$' "${eg[@]}" \
  --set-string gateway.mcp.koinon.egress.caBundle=koinon-test-not-a-real-ca "${eg_gw[@]}"

# koinon-agentd: the URL, the seed only, the key ID and the issuer.
env_is KOINON_EGRESS_URL '"http://koinon-mcp-gateway.koinon.svc.cluster.local:8081"' "${eg[@]}"
env_is KOINON_ACTOR_TOKEN_KEY_FILE '/secrets/actor-token/seed' "${eg[@]}"
env_is KOINON_ACTOR_TOKEN_KEY_ID '"k1"' "${eg[@]}"
env_is KOINON_ACTOR_TOKEN_ISSUER '"koinon-agentd"' "${eg[@]}"
has "agentd mounts the seed of the key Secret" 'key: seed$' "${eg[@]}" "${eg_agentd[@]}"
lacks "agentd never mounts the public key file" 'public.pem' "${eg[@]}" "${eg_agentd[@]}"
n=$(render "${eg[@]}" | grep -c 'secretName: koinon-actor-token-key$' || true)
[ "$n" = 2 ] || fail "the actor token key Secret is mounted in $n places, not 2 (agentd and the gateway)"

# The key: the ensure-secrets Job makes an Ed25519 key once; a supplied Secret replaces it.
has "the ensure-secrets Job makes the actor token key" 'ed25519-key' "${eg[@]}" --show-only templates/ensure-secrets.yaml
lacks "a supplied actor token Secret is not generated" 'ed25519-key' "${eg[@]}" \
  --set gateway.mcp.koinon.egress.actorToken.existingSecret=mine-actor-key --show-only templates/ensure-secrets.yaml
has "a supplied actor token Secret is mounted" 'secretName: mine-actor-key$' "${eg[@]}" \
  --set gateway.mcp.koinon.egress.actorToken.existingSecret=mine-actor-key "${eg_gw[@]}"

# The reconciler adds the enforced L7 sandbox rule for the listener.
has "the reconciler is told the egress URL" 'egressUrl: http://koinon-mcp-gateway.koinon.svc.cluster.local:8081$' "${eg[@]}" --show-only templates/config.yaml

# Network: sandboxes reach the egress port (as agentgateway's llm port), the MCP port stays koinon-api's only.
has "sandboxes may reach the egress port" '^        - port: egress$' "${eg[@]}" --show-only templates/networkpolicy.yaml
lacks "no egress port in the network policy by default" 'port: egress$' --set gateway.mcp.driver=koinon --show-only templates/networkpolicy.yaml
