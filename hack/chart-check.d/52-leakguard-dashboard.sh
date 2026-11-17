# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the leak-guard Grafana dashboard row (issue 1078, ADR 0091, ADR 0023).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

gd=(--set metrics.grafanaDashboard.enabled=true --show-only templates/grafana-dashboard.yaml)
lacks "no dashboard ConfigMap by default" 'koinon-leak-guard\.json'
has "dashboard ConfigMap carries the sidecar label" 'grafana_dashboard: "1"$' "${gd[@]}"
has "dashboard ConfigMap labels follow metrics.grafanaDashboard.labels" 'my-sidecar: dash$' "${gd[@]}" --set metrics.grafanaDashboard.labels.my-sidecar=dash
has "dashboard has the leak-guard row" '"title": "Leak guard"' "${gd[@]}"
has "dashboard has a datasource variable" '"uid": "\$\{datasource\}"' "${gd[@]}"
has "panel: decisions by verdict" 'sum by \(decision\) \(increase\(koinon_api_mcp_guard_decisions_total\[' "${gd[@]}"
has "panel: refusals by reason" 'sum by \(reason\) \(increase\(koinon_api_mcp_guard_decisions_total\{decision=\\"refuse\\"\}' "${gd[@]}"
has "panel: runs over the byte threshold" 'increase\(koinon_api_mcp_guard_byte_threshold_runs_total\[' "${gd[@]}"
n=$(render "${gd[@]}" | grep -c '"type": "row"' || true)
[ "$n" = 1 ] || fail "the dashboard has $n rows, want 1 (one leak-guard row)"
# The panel metric names exist in the code, and no query uses an ID label (hack/cmd/dashboardmetrics).
hack/dashboard-metrics-check.sh >/dev/null || fail "hack/dashboard-metrics-check.sh failed: a panel metric is not in the code, or a query uses an ID label"
