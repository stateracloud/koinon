# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the Helm-set platform settings koinon-api records on start (ADR 0100 decision 3,
# #331). Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# koinon-api records the chart's version; the chart is the only place that knows it.
chart_version=$(awk '/^version:/ {gsub(/"/, "", $2); print $2}' "$CHART/Chart.yaml")
[[ -n "$chart_version" ]] || fail "could not read the chart version from Chart.yaml"
# has matches with grep -E: escape the regex characters of the version (the "." and the "+" of a build tag).
# shellcheck disable=SC2016 # the sed program is a literal bracket expression, not a shell expansion
chart_version_re=$(sed 's/[][\.*+?^$(){}|/-]/\\&/g' <<<"$chart_version")
has "koinon-api is told the chart version to record" "\"serve\", \"--chart-version\", \"$chart_version_re\""
