# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the MCP leak guard (ADR 0084, ADR 0091, old issue 349).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# koinon-api serves the guard on its own port and Service; the reconciler renders that Service's
# address into every org MCP route's mcpGuardrails (failClosed). Without the address the gateway
# refuses every org MCP call, so the chart must always pass it.
k8s=(--set gateway.mode=kubernetes)
has "koinon-api serves the leak guard" '"--guard-addr", ":8083"' --show-only templates/deployments.yaml
has "koinon-api passes the byte cap" '"--leak-guard-byte-cap", "2048"' --show-only templates/deployments.yaml
has "api.leakGuardByteCap is used" '"--leak-guard-byte-cap", "4096"' --set api.leakGuardByteCap=4096 --show-only templates/deployments.yaml
has "koinon-api names the guard port" 'name: guard$' --show-only templates/deployments.yaml
has "the guard has its own Service" 'name: koinon-api-guard$' --show-only templates/deployments.yaml
has "reconciler config carries the guard URL" 'url: http://koinon-api-guard\.koinon\.svc:8083$' --show-only templates/config.yaml
has "reconciler config carries the guard URL (gateway.mode=kubernetes)" 'url: http://koinon-api-guard\.koinon\.svc:8083$' "${k8s[@]}" --show-only templates/config.yaml
has "api.guardPort is used" 'url: http://koinon-api-guard\.koinon\.svc:9443$' --set api.guardPort=9443 --show-only templates/config.yaml

# The guard trusts the agent the gateway names in its metadata, so only the gateway may reach its
# port: the api's ingress rule for port guard admits the gateway's pods and nothing else, and the
# gateway's egress reaches the port (the private ranges are carved out of its internet egress).
guard_from() {
	local out
	out=$(render --show-only templates/networkpolicy.yaml "$@")
	awk '/- port: guard$/{on=1; next} on && /^    (#|- ports:)/{exit} on' <<<"$out"
}
from=$(guard_from)
grep -q 'app.kubernetes.io/component: gateway$' <<<"$from" || fail "the guard port admits the standalone gateway"
from=$(guard_from "${k8s[@]}")
grep -q 'gateway.networking.k8s.io/gateway-name: koinon-gateway$' <<<"$from" || fail "the guard port admits the controller's proxy (gateway.mode=kubernetes)"
for args in "" "gateway.mode=kubernetes"; do
	from=$(guard_from ${args:+--set "$args"})
	n=$(grep -c -- '- podSelector:' <<<"$from" || true)
	[ "$n" = 1 ] || fail "the guard port admits $n pod selectors with '$args', want the gateway's only"
	if grep -Eq 'namespaceSelector|ipBlock|component: (agentd|reconciler|api)$' <<<"$from"; then
		fail "the guard port admits more than the gateway with '$args'"
	fi
done
n=$(render --show-only templates/networkpolicy.yaml | grep -c -- '- port: 8083$' || true)
[ "$n" = 1 ] || fail "the standalone gateway's egress reaches the guard port $n times, want 1"
n=$(render --show-only templates/networkpolicy.yaml "${k8s[@]}" | grep -c -- '- port: 8083$' || true)
[ "$n" = 1 ] || fail "the proxy's egress reaches the guard port $n times, want 1 (gateway.mode=kubernetes)"

# The guard needs no Kubernetes permission: it is a gRPC listener of koinon-api, and agentd's taint
# feed writes fingerprints to the database. Neither ServiceAccount gains a rule for it.
lacks "no RBAC rule for the leak guard" 'guard' --show-only templates/rbac.yaml
lacks "no RBAC rule for the leak guard (gateway.mode=kubernetes)" 'guard' "${k8s[@]}" --show-only templates/rbac.yaml
