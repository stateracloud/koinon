# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the Substrate control plane reaches the gateway (issue #1469), and Substrate refuses
# gateway.mode=kubernetes (issue #1489). atenet-egress carries the actors' model and MCP calls to the gateway and adds an
# actor JWT that only koinon-gateway checks. koinon-gateway is a chart pod only in standalone mode, so Substrate needs
# gateway.mode=standalone; the kubernetes mode is not supported with Substrate in v0.1.0 (ADR 0097 amendment of
# 2026-11-27) and the chart refuses the pair. Sourced in order by hack/chart-check.sh; it uses that script's helpers
# (render, fail, ...) and docof and nocomment of 44-substrate-worker.sh.

# sgw_control: the koinon-substrate-control policy of a render with ci/substrate-values.yaml, without
# comment lines (a comment must not count as a peer or a port).
sgw_control() {
	render -f "$CHART/ci/substrate-values.yaml" --show-only templates/substrate-networkpolicy.yaml |
		docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-control\n' | nocomment
}

# Standalone mode: the control plane egress reaches the chart's gateway pods, and selects no controller's proxy.
sgw_std=$(sgw_control)
[ -n "$(awk '/component: gateway$/' <<<"$sgw_std")" ] || fail "standalone: the Substrate control plane egress does not reach the gateway"
if grep -q 'gateway.networking.k8s.io/gateway-name' <<<"$sgw_std"; then fail "standalone: the Substrate control plane egress selects a controller's proxy"; fi

# Kubernetes mode: refused, and the error names the fix.
if render -f "$CHART/ci/substrate-values.yaml" --set gateway.mode=kubernetes --set gateway.mcp.driver=agentgateway \
	>/dev/null 2>"$out/sgw-k8s.err"; then
	fail "Substrate with gateway.mode=kubernetes must not render"
else
	grep -q 'gateway.mode=standalone' "$out/sgw-k8s.err" || fail "the refusal of Substrate in kubernetes mode does not name gateway.mode=standalone"
	grep -q 'gateway.mode=kubernetes is not supported with execution.driver=substrate in v0.1.0' "$out/sgw-k8s.err" || fail "the refusal of Substrate in kubernetes mode does not say it is unsupported in v0.1.0"
fi

# Default driver: with Substrate and no gateway.model.driver, the chart uses koinon (ADR 0097 section 16). The null
# removes the fixture's explicit value.
sgw_def=$(render -f "$CHART/ci/substrate-values.yaml" --set gateway.model.driver=null) || fail "Substrate without gateway.model.driver does not render"
hit 'name: koinon-model-gateway' <<<"$sgw_def" || fail "Substrate without gateway.model.driver does not render koinon-model-gateway"
hit 'modelUrl: http://koinon-model-gateway' <<<"$sgw_def" || fail "Substrate without gateway.model.driver does not set the reconciler modelUrl to koinon-model-gateway"
