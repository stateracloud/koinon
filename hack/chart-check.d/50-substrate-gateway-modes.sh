# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the Substrate control plane reaches the gateway in both gateway modes (issue #1469). atenet-egress
# carries the actors' model and MCP calls to the gateway. In standalone mode the gateway is a chart pod (Koinon's
# selector labels). In kubernetes mode the agentgateway controller runs the proxy, whose pods have only the label
# gateway.networking.k8s.io/gateway-name (ADR 0058), so the control-plane egress must select that label, on the
# Gateway's listener port only (no all-ports rule, as for koinon-gateway in issue #1372). Sourced in order by
# hack/chart-check.sh; it uses that script's helpers (render, fail, ...).

sgw_k8s=(--set gateway.mode=kubernetes --set gateway.mcp.driver=agentgateway)
sgw_proxy='gateway.networking.k8s.io/gateway-name: koinon-gateway'

# sgw_control <render args...>: the koinon-substrate-control policy of a render with ci/substrate-values.yaml.
sgw_control() {
	render -f "$CHART/ci/substrate-values.yaml" "$@" --show-only templates/substrate-networkpolicy.yaml |
		awk 'BEGIN { RS = "\n---\n" } $0 ~ /\n  name: koinon-substrate-control\n/ { print }'
}
# sgw_rules <pattern> <rendered policy>: each egress rule with a peer line that matches the pattern (an awk regex), each
# ended by a line "@@@". A rule ends at the next rule, at the end of the egress list or at a document boundary.
sgw_rules() { awk -v pat="$1" '
function flush() { if (buf ~ pat) printf "%s@@@\n", buf; buf = "" }
/^---$/ { flush(); f = 0; next }
/^  egress:/ { flush(); f = 1; next }
f && /^    - / { flush(); buf = $0 "\n"; next }
f && /^  [a-z]/ { flush(); f = 0; next }
f { buf = buf $0 "\n" }
END { flush() }' <<<"$2"; }
# sgw_proxy_problem <rendered policy> <listener port>: prints why the egress to the controller's proxy is wrong, or
# nothing. Exactly one rule selects the proxy, it lists ports, and its only port is the listener port.
sgw_proxy_problem() {
	local rules n ports
	rules=$(sgw_rules "$sgw_proxy" "$1")
	n=$(grep -c '^@@@$' <<<"$rules" || true)
	[ "$n" = 1 ] || { echo "$n egress rules reach the controller's proxy, want exactly one"; return; }
	grep -q '^      ports:' <<<"$rules" || { echo "the egress rule for the controller's proxy has no ports: every port would be open"; return; }
	ports=$(grep -oE 'port: [^,}]+' <<<"$rules" | sort -u | tr '\n' ' ')
	[ "$ports" = "port: $2 " ] || echo "the egress rule for the controller's proxy opens '${ports% }', want only 'port: $2'"
}

# The helper itself: an all-ports rule, a second rule, a missing peer and an extra port are all reported.
sgw_fake() { printf '%s\n' 'kind: NetworkPolicy' 'metadata:' '  name: koinon-substrate-control' 'spec:' '  egress:' "$@"; }
sgw_peer=('    - to:' '        - podSelector:' '            matchLabels:' "              $sgw_proxy")
[ -z "$(sgw_proxy_problem "$(sgw_fake "${sgw_peer[@]}" '      ports:' '        - {port: 4000, protocol: TCP}')" 4000)" ] ||
	fail "sgw_proxy_problem reports a correct proxy rule"
[ -n "$(sgw_proxy_problem "$(sgw_fake "${sgw_peer[@]}")" 4000)" ] || fail "sgw_proxy_problem misses an all-ports proxy rule"
[ -n "$(sgw_proxy_problem "$(sgw_fake '    - to:' '        - podSelector: {}')" 4000)" ] || fail "sgw_proxy_problem misses a missing proxy peer"
[ -n "$(sgw_proxy_problem "$(sgw_fake "${sgw_peer[@]}" '      ports:' '        - {port: 4000, protocol: TCP}' "${sgw_peer[@]}")" 4000)" ] ||
	fail "sgw_proxy_problem misses a second (all-ports) proxy rule"
[ -n "$(sgw_proxy_problem "$(sgw_fake "${sgw_peer[@]}" '      ports:' '        - {port: 4000, protocol: TCP}' '        - {port: 15020, protocol: TCP}')" 4000)" ] ||
	fail "sgw_proxy_problem misses the stats port on the proxy rule"

# Standalone mode: the chart's gateway pods, and no peer for a controller's proxy.
sgw_std=$(sgw_control)
[ -n "$(sgw_rules 'component: gateway\n' "$sgw_std")" ] || fail "standalone: the Substrate control plane egress does not reach the gateway"
if grep -q "$sgw_proxy" <<<"$sgw_std"; then fail "standalone: the Substrate control plane egress selects a controller's proxy"; fi

# Kubernetes mode: the controller's proxy on the listener port, and no peer that selects no pod.
sgw_k=$(sgw_control "${sgw_k8s[@]}")
[ -n "$sgw_k" ] || fail "kubernetes mode with Substrate renders no koinon-substrate-control policy"
sgw_p=$(sgw_proxy_problem "$sgw_k" 4000)
[ -z "$sgw_p" ] || fail "kubernetes mode with Substrate: $sgw_p"
if grep -q 'component: gateway$' <<<"$sgw_k"; then fail "kubernetes mode: the Substrate control plane egress selects the chart's gateway pods, which do not exist"; fi
grep -q 'component: api$' <<<"$sgw_k" || fail "kubernetes mode: the Substrate control plane egress does not reach koinon-api"
sgw_p=$(sgw_proxy_problem "$(sgw_control "${sgw_k8s[@]}" --set gateway.kubernetes.port=4001)" 4001)
[ -z "$sgw_p" ] || fail "kubernetes mode with gateway.kubernetes.port=4001: $sgw_p"
# The other fence: the proxy's own policy admits the Substrate namespace on the same port.
sgw_ing=$(render -f "$CHART/ci/substrate-values.yaml" "${sgw_k8s[@]}" --show-only templates/networkpolicy.yaml |
	awk 'BEGIN { RS = "\n---\n" } $0 ~ /\n  name: koinon-gateway\n/ { print }' | awk '/^  ingress:/{f=1} f' | awk '/- port: 4000$/{f=1} f&&n++<6')
grep -q 'kubernetes.io/metadata.name: ate-system$' <<<"$sgw_ing" ||
	fail "kubernetes mode: the proxy's policy does not admit the Substrate namespace on its listener port"
