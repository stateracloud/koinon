# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: clusterDomain reaches the connector URL check of koinon-api and koinon-reconciler (#1054).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# The check accepts a Service name only in its full <service>.<namespace>.svc.<domain> form, so both binaries
# must read the chart's domain and not their built-in default. Exactly two pods read it: the API and the
# reconciler (agentd checks no connector URL).
cluster_domain_pods() { # <helm args...>: the number of rendered KOINON_CLUSTER_DOMAIN entries with the value $want
	render --show-only templates/deployments.yaml "$@" | grep -A1 -E -- '- name: KOINON_CLUSTER_DOMAIN$' | grep -Fc -- "value: $want" || true
}
want='"cluster.local"'
n=$(cluster_domain_pods)
[ "$n" = 2 ] || fail "clusterDomain default: $n pods read KOINON_CLUSTER_DOMAIN=cluster.local, want 2 (api and reconciler)"
want='"corp.example"'
n=$(cluster_domain_pods --set clusterDomain=corp.example)
[ "$n" = 2 ] || fail "clusterDomain=corp.example: $n pods read KOINON_CLUSTER_DOMAIN=corp.example, want 2 (api and reconciler)"
n=$(render --show-only templates/deployments.yaml --set clusterDomain=corp.example | grep -Fc -- 'KOINON_CLUSTER_DOMAIN' || true)
[ "$n" = 2 ] || fail "KOINON_CLUSTER_DOMAIN is set in $n pods, want only api and reconciler"
# The same value still builds the chart's own full Service names.
has "clusterDomain builds the gateway's full name" 'koinon-gateway\.koinon\.svc\.corp\.example' --set clusterDomain=corp.example
# The schema refuses an empty value, so the binaries never see one from the chart.
if render_raw --set clusterDomain= >/dev/null 2>&1; then fail "an empty clusterDomain must not render"; fi
