#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Dashboard metrics check (issue 1078, ADR 0023): every metric name in a panel query of the chart's
# Grafana dashboards is registered in the Go code, and no query uses an ID label. The logic is Go:
# hack/cmd/dashboardmetrics (ADR 0118), a main-module tool that `go tool` runs, so its exit codes pass
# through (0 clean, 1 findings, 2 usage or read error).
#
# Usage: hack/dashboard-metrics-check.sh    (checks charts/koinon/dashboards/*.json against the Go code)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"
exec go tool dashboardmetrics "$repo" charts/koinon/dashboards/*.json
