# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the OpenShell channel TLS, key-encryption keys, DBOS, gateway egress.
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# The OpenShell channel needs mTLS (koinon-private#103): without a client TLS Secret the chart renders only with
# an explicit openshell.insecure=true, never with both, and each choice reaches the components.
if render_raw >/dev/null 2>"$out/tls.err"; then fail "no client TLS Secret and no insecure must not render"; fi
grep -q 'openshell.clientTLSSecret is required' "$out/tls.err" && grep -q 'openshell.insecure=true' "$out/tls.err" || fail "the TLS error must name both openshell.clientTLSSecret and openshell.insecure"
render_raw --set openshell.clientTLSSecret=tls >/dev/null 2>&1 || fail "a client TLS Secret alone must render"
render_raw --set openshell.insecure=true >/dev/null 2>&1 || fail "openshell.insecure=true alone must render"
if render_raw --set openshell.clientTLSSecret=tls --set openshell.insecure=true >"$out/both.out" 2>"$out/both.err"; then fail "a client TLS Secret together with insecure must not render"; fi
grep -q 'contradict' "$out/both.err" || fail "the TLS-plus-insecure error must say the options contradict"
# openshell.address has no default: without it the reconciler and agentd would crash-loop.
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true >/dev/null 2>"$out/addr.err"; then fail "no openshell.address must not render"; fi
grep -q 'openshell.address is required' "$out/addr.err" || fail "the missing-address error must name openshell.address"
has "openshell.address reaches the reconciler config" 'address: "openshell.openshell.svc:8080"' --show-only templates/config.yaml
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
lacks "the DBOS runtime is off by default" '"--durable"' --show-only templates/deployments.yaml
lacks "the migration Job leaves the DBOS schema alone by default" '"--durable"' --show-only templates/migrate-job.yaml
has "api.durable.enabled makes the migration Job create the DBOS tables" '"migrate", "--wait", "4m", "--durable"' --set api.durable.enabled=true --show-only templates/migrate-job.yaml
has "api.durable.enabled starts the DBOS runtime" '"--durable"' --set api.durable.enabled=true --show-only templates/deployments.yaml
# The gateway mounts the provider keys Secret, and that Secret only.
has "gateway mounts the provider keys" 'mountPath: /secrets/providers$' --show-only templates/gateway-standalone.yaml
n=$(render | grep -c 'secretName: koinon-gateway-providers$' || true)
[ "$n" = 1 ] || fail "the provider keys Secret is mounted by $n pods, want only the gateway"
# The gateway's egress is restricted by a standard NetworkPolicy; internal upstreams are added by value.
has "gateway egress policy" 'policyTypes: \[Ingress, Egress\]' --show-only templates/networkpolicy.yaml
has "gateway egress carves out the private ranges" '10.0.0.0/8' --show-only templates/networkpolicy.yaml
lacks "no egress policy with networkPolicy.enabled=false" 'policyTypes: \[Ingress, Egress\]' --set networkPolicy.enabled=false
has "gatewayEgressTo is rendered" 'cidr: 10.7.0.0/24' --set 'networkPolicy.gatewayEgressTo[0].to[0].ipBlock.cidr=10.7.0.0/24' --show-only templates/networkpolicy.yaml
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
