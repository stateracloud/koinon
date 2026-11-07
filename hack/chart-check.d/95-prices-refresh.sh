# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the price refresh (ADR 0098).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# Off by default: koinon-api is not told to fetch, so it makes no request to a price source.
lacks "no price refresh flag by default" 'prices-refresh'
# NOTES are printed by install, not by template.
notes_off=$("$HELM" install koinon "$CHART" -n koinon --dry-run=client --set openshell.insecure=true --set "$ADDRESS" 2>&1) || true
grep -q 'prices.refresh is off' <<<"$notes_off" || fail "no NOTES line when the price refresh is off"
notes_on=$("$HELM" install koinon "$CHART" -n koinon --dry-run=client --set openshell.insecure=true --set "$ADDRESS" --set prices.refresh.enabled=true 2>&1) || true
if grep -q 'prices.refresh is off' <<<"$notes_on"; then fail "NOTES say the refresh is off when it is on"; fi
has "enabled, koinon-api is told to refresh" '"--prices-refresh-enabled"' --set prices.refresh.enabled=true
has "the interval is passed" '"--prices-refresh-interval", "6h"' --set prices.refresh.enabled=true --set prices.refresh.interval=6h
if render --set prices.refresh.interval=daily >/dev/null 2>&1; then fail "a prices.refresh.interval that is not a duration of hours or minutes must not render"; fi
