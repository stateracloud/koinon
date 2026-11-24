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
		# An empty `to` list (`to: []`, or a `to:` key with no list item) matches every destination: it is no `to`.
		function flush() { if (inrule && !hasto && name != "") bad[name] = 1; inrule = 0; hasto = 0; topend = 0 }
		function seto(line) { if (line ~ /to:[ \t]*(\[\][ \t]*)?(#.*)?$/) { topend = 1; hasto = 0 } else hasto = 1 }
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

# These policies of ci/substrate-values.yaml carry a destination-less egress rule, each on purpose:
#   koinon-gateway            the gateway's DNS rule (also on main)
#   koinon-substrate-otel-prometheus     Prometheus of otel-system: the API server ports
#   koinon-substrate-podcert-controller  the certificate controller: the API server ports
#   koinon-substrate-api-server          atelet, ate-controller, ate-api-server, atenet-router: the API server ports
#   koinon-substrate-api-only            k8s-credential-provider: the API server ports
# Check the names of the chart against this list with the render of the file.
expected=$'koinon-gateway\nkoinon-substrate-api-only\nkoinon-substrate-api-server\nkoinon-substrate-otel-prometheus\nkoinon-substrate-podcert-controller'
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
