# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the fence of otel-system and podcertificate-controller-system, and the telemetry route of the
# control plane (issue #1172, follows #1089). Sourced in order by hack/chart-check.sh; it uses that script's helpers
# (render, has, lacks, fail, ...) and the wsub and docof helpers of 44-substrate-worker.sh.

fence=(--show-only templates/substrate-fence-networkpolicy.yaml)
lacks "no fence with the openshell driver" 'substrate-(otel|podcert|telemetry)'
fdocs=$(render "${wsub[@]}" "${fence[@]}")
for n in telemetry otel-default-deny otel-collector otel-trace-backend otel-prometheus podcert-default-deny podcert-controller; do
	has "fence policy $n" "name: koinon-substrate-$n\$" "${wsub[@]}"
done
# Both fenced namespaces deny ingress and egress by default.
for n in otel-default-deny podcert-default-deny; do
	d=$(docof "name: koinon-substrate-$n\n" <<<"$fdocs")
	grep -Fq 'podSelector: {}' <<<"$d" || fail "$n must select every pod"
	grep -Fq 'policyTypes: [Ingress, Egress]' <<<"$d" || fail "$n must deny ingress and egress"
	if grep -Eq '^  (ingress|egress):' <<<"$d"; then fail "$n must allow nothing"; fi
done
# The control plane reaches the collector on the OTLP ports only, to the collector pods only.
tel=$(docof "name: koinon-substrate-telemetry\n" <<<"$fdocs")
grep -Fq 'kubernetes.io/metadata.name: "otel-system"' <<<"$tel" || fail "the telemetry route must name the otel-system namespace"
grep -Fq 'app: opentelemetry-collector' <<<"$tel" || fail "the telemetry route must select the collector pods"
if [ "$(grep -c 'port: ' <<<"$tel")" != 2 ] || ! grep -Fq 'port: 4317' <<<"$tel" || ! grep -Fq 'port: 4318' <<<"$tel"; then fail "the telemetry route must open the two OTLP ports only"; fi
for a in atelet ate-controller ate-api-server atenet-router atenet-egress; do
	grep -Eq "^ +- $a\$" <<<"$tel" || fail "the telemetry route must cover $a"
done
# The collector takes OTLP from the control plane and its read ports from Prometheus only; the trace backend takes OTLP from the collector only.
col=$(docof "name: koinon-substrate-otel-collector\n" <<<"$fdocs")
grep -Fq 'kubernetes.io/metadata.name: "ate-system"' <<<"$col" || fail "the collector must take OTLP from the control-plane namespace"
grep -Fq 'app: "prometheus"' <<<"$col" || fail "the collector read ports must be open to Prometheus only"
grep -Fq 'port: 8889' <<<"$col" || fail "the collector read port 8889 must be open to Prometheus"
back=$(docof "name: koinon-substrate-otel-trace-backend\n" <<<"$fdocs")
grep -Fq 'app: "opentelemetry-collector"' <<<"$back" || fail "the trace backend must take OTLP from the collector only"
if grep -Fq 'egress' <<<"$back"; then fail "the trace backend must have no egress rule"; fi
# The identity-certificate controller reaches the API server only and takes no ingress.
pcd=$(docof "name: koinon-substrate-podcert-controller\n" <<<"$fdocs")
grep -Fq 'cidr: "192.0.2.10/32"' <<<"$pcd" || fail "the podcert controller must reach the API server CIDRs"
if grep -Eq '^  ingress:|namespaceSelector|podSelector: \{\}' <<<"$pcd"; then fail "the podcert controller must reach the API server only"; fi
# Switches and failures.
lacks "no otel fence when switched off" 'otel-default-deny' "${wsub[@]}" --set execution.substrate.workerNetworkPolicy.otelSystem.enabled=false
lacks "no podcert fence when switched off" 'podcert-default-deny' "${wsub[@]}" --set execution.substrate.workerNetworkPolicy.podcertSystem.enabled=false
lacks "no telemetry route when switched off" 'substrate-telemetry' "${wsub[@]}" --set execution.substrate.workerNetworkPolicy.telemetry.enabled=false
has "the collector namespace is configurable" 'kubernetes.io/metadata.name: "mon"' "${wsub[@]}" --set execution.substrate.workerNetworkPolicy.telemetry.namespace=mon
if render "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.telemetry.ports={}' "${fence[@]}" >/dev/null 2>&1; then fail "an empty telemetry port list must fail the render (an empty list opens every port)"; fi
if render "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.telemetry.podSelector=null' "${fence[@]}" >/dev/null 2>&1; then fail "an empty telemetry selector must fail the render"; fi
