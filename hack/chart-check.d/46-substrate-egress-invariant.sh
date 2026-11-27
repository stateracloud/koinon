# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertion: the egress rules WITHOUT a destination (`to`) of every rendered NetworkPolicy (ADR 0120 amendment
# 10, issue #1172). A rule with ports and no `to` opens those ports to ANY address, so the set of policies that carry
# one is a security fact: it must equal the expected list, and a new policy with such a rule fails this check until
# someone reads it and adds its name here. Sourced in order by hack/chart-check.sh; it uses that script's helpers and
# the $out directory (with the render of ci/substrate-values.yaml, made at the top of the script).

# toless_policies: the names of the NetworkPolicies of a rendered chart on stdin that have an egress rule without `to`.
toless_policies() {
	awk '
		# An empty `to` list (`to: []`, `to: [ ]`, `to: null`, `to: ~`, or a `to:` key with no list item) matches every destination: it is no `to`.
		function flush() { if (inrule && !hasto && name != "") bad[name] = 1; inrule = 0; hasto = 0; topend = 0 }
		function seto(line) { if (line ~ /to:[ \t]*(\[[ \t]*\]|null|~)?[ \t]*(#.*)?$/) { topend = 1; hasto = 0 } else hasto = 1 }
		function doc_end() { flush(); inegress = 0; name = ""; isnp = 0 }
		/^---$/ { doc_end(); next }
		/^kind: NetworkPolicy$/ { isnp = 1 }
		/^  name: / && name == "" { name = $2; gsub(/"/, "", name) }
		!isnp { next }
		/^  egress:/ { inegress = 1; next }
		/^  [A-Za-z]/ { flush(); inegress = 0; next }
		inegress && /^    - / { flush(); inrule = 1; if ($0 ~ /^    - to:/) seto($0); next }
		inegress && inrule && /^      to:/ { seto($0); next }
		inegress && inrule && topend && /^ +- / { hasto = 1; topend = 0; next }
		inegress && inrule && /^      [A-Za-z]/ { topend = 0 }
		END { doc_end(); for (n in bad) print n }
	' | sort
}
# A `to`-less rule in a made-up policy is found, and a rule with `to` is not (the helper itself is tested).
[[ $(printf '%s\n' 'kind: NetworkPolicy' 'metadata:' '  name: a' 'spec:' '  egress:' '    - ports:' '        - {port: 1, protocol: TCP}' '    - to:' '        - podSelector: {}' '---' 'kind: NetworkPolicy' 'metadata:' '  name: b' 'spec:' '  egress:' '    - ports:' '        - {port: 53}' '      to:' '        - podSelector: {}' | toless_policies) == a ]] || fail "toless_policies must report a policy with a rule without to, and only that"
# An empty `to` list matches every destination, so it counts as no `to` (inline, as a first key, or as a bare key).
[[ $(printf '%s\n' 'kind: NetworkPolicy' 'metadata:' '  name: c' 'spec:' '  egress:' '    - to: []' '      ports:' '        - {port: 22}' '---' 'kind: NetworkPolicy' 'metadata:' '  name: d' 'spec:' '  egress:' '    - ports:' '        - {port: 22}' '      to: []' '---' 'kind: NetworkPolicy' 'metadata:' '  name: e' 'spec:' '  egress:' '    - to:' '      ports:' '        - {port: 22}' '---' 'kind: NetworkPolicy' 'metadata:' '  name: f' 'spec:' '  egress:' '    - to:' '        - podSelector: {}' '      ports:' '        - {port: 22}' | toless_policies) == $'c\nd\ne' ]] || fail "toless_policies must treat an empty to list as no to"
# `to: null`, `to: ~` and `to: [ ]` are an empty destination too (Helm renders a nil list as `to: null`).
[[ $(printf '%s\n' 'kind: NetworkPolicy' 'metadata:' '  name: g' 'spec:' '  egress:' '    - to: null' '      ports:' '        - {port: 22}' '---' 'kind: NetworkPolicy' 'metadata:' '  name: h' 'spec:' '  egress:' '    - ports:' '        - {port: 22}' '      to: ~' '---' 'kind: NetworkPolicy' 'metadata:' '  name: i' 'spec:' '  egress:' '    - ports:' '        - {port: 22}' '      to: [ ]  # none' | toless_policies) == $'g\nh\ni' ]] || fail "toless_policies must treat to: null, to: ~ and to: [ ] as no to"

# These policies of ci/substrate-values.yaml carry a destination-less egress rule, each on purpose:
#   koinon-gateway            the gateway's DNS rule (also on main)
#   koinon-mcp-gateway        koinon-gateway's DNS rule (the MCP path, the default driver since #1239)
#   koinon-model-gateway      koinon-gateway's DNS rule (the model path, which Substrate requires since #1489)
#   koinon-substrate-otel-prometheus    Prometheus of otel-system: the API server ports
#   koinon-substrate-podcert-controller  the certificate controller: the API server ports
#   koinon-substrate-api-server          atelet, ate-controller, ate-api-server, atenet-router: the API server ports
#   koinon-substrate-api-only            k8s-credential-provider: the API server ports
# Check the names of the chart against this list with the render of the file.
expected=$'koinon-gateway\nkoinon-mcp-gateway\nkoinon-model-gateway\nkoinon-substrate-api-only\nkoinon-substrate-api-server\nkoinon-substrate-otel-prometheus\nkoinon-substrate-podcert-controller'
got=$(toless_policies <"$out/substrate-values.yaml")
[[ $got == "$expected" ]] || fail "the NetworkPolicies with an egress rule without a destination differ from the expected set (a rule with ports and no to opens them to ANY address; ADR 0120 amendment 10). Expected: $(tr '\n' ' ' <<<"$expected")| got: $(tr '\n' ' ' <<<"$got")"
# The workers policy has no such rule at all, and neither has the control policy (atenet-egress is selected by it).
for n in koinon-substrate-workers koinon-substrate-control koinon-substrate-default-deny-egress; do
	if grep -Fxq "$n" <<<"$got"; then fail "the policy $n must have no egress rule without a destination"; fi
done
wk=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-workers\n' <<<"$(render "${wsub[@]}" --show-only templates/substrate-networkpolicy.yaml)")
[[ -n $wk ]] || fail "the workers policy must render"
[[ $(toless_policies <<<"$wk") == "" ]] || fail "the workers policy must have no egress rule without a destination (workers get no route to the API server)"
# The egress pods have no API server client: listing their app in apiServerApps fails the render.
if render "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.apiServerApps={atelet,atenet-egress}' --show-only templates/substrate-networkpolicy.yaml >/dev/null 2>"$out/egressapp.err"; then fail "apiServerApps with atenet-egress must not render"; fi
grep -Fq 'apiServerApps lists "atenet-egress"' "$out/egressapp.err" || fail "the error for atenet-egress in apiServerApps must name apiServerApps and the app"
# An extraEgress rule with an empty `to` list opens its ports to ANY address: the invariant must catch it in the real render.
emp=$(render "${wsub[@]}" --set-json 'execution.substrate.workerNetworkPolicy.extraEgress=[{"to":[],"ports":[{"port":22}]}]' --show-only templates/substrate-networkpolicy.yaml) || fail "the render with an extraEgress rule with an empty to must succeed"
# The render without extraEgress already reports the apiServerPorts policies on purpose, so test for the policy that the rule adds.
[[ $(toless_policies <<<"$emp") == $'koinon-substrate-api-only\nkoinon-substrate-api-server\nkoinon-substrate-control' ]] || fail "an extraEgress rule with to: [] must make the egress invariant find koinon-substrate-control as a policy without a destination (besides the apiServerPorts policies)"

# Every gateway ingress rule that admits atenet-egress needs the matching egress rule in <rel>-substrate-control (issue
# #1523): atenet-egress carries the actors' model and MCP calls, and the control policy's default-deny egress drops a call
# whose destination pod and port no rule names, even when the gateway's own ingress admits it. The model route was
# missing that rule, so every actor model call timed out on kind. np_pairs reads a render on stdin and prints
# "<kind> <component> <port>" lines: kind "in" for each ingress rule of a policy that has atenet-egress as a peer, kind
# "out" for each egress rule of a policy (with the destination component and port; a rule with no component is skipped).
np_pairs() {
	awk '
		function flush(   i, n, p) {
			if (rule != "") {
				n = split(ports, p, " ")
				for (i = 1; i <= n; i++) {
					if (kind == "ingress" && rule ~ /app: atenet-egress/ && pol_comp != "") print "in " pol_comp " " p[i]
					if (kind == "egress" && name ~ /-substrate-control$/ && dest != "") print "out " dest " " p[i]
				}
			}
			rule = ""; ports = ""; dest = ""
		}
		function doc_end() { flush(); name = ""; isnp = 0; kind = ""; pol_comp = ""; inspec = 0; insel = 0 }
		/^---$/ { doc_end(); next }
		/^kind: NetworkPolicy$/ { isnp = 1 }
		!isnp { next }
		/^  name: / && name == "" { name = $2; gsub(/"/, "", name) }
		/^  podSelector:/ { insel = 1; next }
		insel && /app.kubernetes.io\/component:/ && pol_comp == "" { pol_comp = $2; next }
		/^  [A-Za-z]/ { flush(); insel = 0; kind = ""; if ($1 == "ingress:") kind = "ingress"; if ($1 == "egress:") kind = "egress"; next }
		kind != "" && /^    - / { flush(); rule = "x" }
		kind != "" && rule != "" && $0 !~ /^[ ]*#/ { rule = rule $0 "\n" }
		kind == "egress" && /app.kubernetes.io\/component:/ { dest = $NF }
		kind != "" && /^ +- \{?port: [a-z]/ { v = $0; sub(/.*port: /, "", v); sub(/[,}].*/, "", v); ports = ports " " v }
		END { doc_end() }
	' | sort -u
}
# Run the pairing on the default render and on one with the egress listener (gateway.mcp.koinon.egress.enabled), which
# adds the gateway's egress-listener ingress rule for atenet-egress.
for np_extra in "" "--set gateway.mcp.koinon.egress.enabled=true"; do
	np_args=($np_extra)
	np_all=$(render "${wsub[@]}" "${np_args[@]}")
	pairs=$(np_pairs <<<"$np_all")
	ins=$(grep '^in ' <<<"$pairs" || true)
	[[ -n $ins ]] || fail "the substrate render ($np_extra) must have gateway ingress rules that admit atenet-egress (np_pairs found none)"
	while read -r _ comp port; do
		grep -Fxq "out $comp $port" <<<"$pairs" || fail "the gateway policy of $comp admits atenet-egress on port $port, but koinon-substrate-control has no egress rule to that pod and port (issue #1523; render: ${np_extra:-default}): the actors' calls would time out"
	done <<<"$ins"
done
