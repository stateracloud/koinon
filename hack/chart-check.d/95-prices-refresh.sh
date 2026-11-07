# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the price refresh (ADR 0098, decision 2.2 as amended on 2026-11-07).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# Three modes from prices.refresh.enabled, passed as one flag: unset is auto, false is off, true is on.
# The chart knows nothing about providers; koinon-api decides in auto mode which sources to fetch.
has "unset, koinon-api refreshes in auto mode" '"--prices-refresh", "auto"'
has "enabled=false, koinon-api refreshes nothing" '"--prices-refresh", "off"' --set prices.refresh.enabled=false
has "enabled=true, koinon-api refreshes from every source" '"--prices-refresh", "on"' --set prices.refresh.enabled=true
lacks "the old boolean flag is gone" 'prices-refresh-enabled' --set prices.refresh.enabled=true
lacks "the chart passes no provider list" 'prices-refresh-(sources|providers)'
has "the interval is passed" '"--prices-refresh-interval", "6h"' --set prices.refresh.interval=6h
if render --set prices.refresh.interval=0h >/dev/null 2>&1; then fail "a prices.refresh.interval of 0h must not render"; fi
if render --set prices.refresh.interval=30m >/dev/null 2>&1; then fail "a prices.refresh.interval below one hour must not render"; fi
if render --set prices.refresh.interval=daily >/dev/null 2>&1; then fail "a prices.refresh.interval that is not a duration of hours or minutes must not render"; fi
if render --set prices.refresh.enabled=auto >/dev/null 2>&1; then fail "prices.refresh.enabled takes true, false or null, not a string"; fi

# The auto-apply bounds: left to koinon-api's defaults unless set, then within their ranges.
lacks "no bound flags by default" 'prices-refresh-max-(change-percent|auto-per-day)'
has "maxChangePercent is passed" '"--prices-refresh-max-change-percent", "10"' --set prices.refresh.maxChangePercent=10
has "maxAutoPerDay is passed" '"--prices-refresh-max-auto-per-day", "3"' --set prices.refresh.maxAutoPerDay=3
if render --set prices.refresh.maxChangePercent=26 >/dev/null 2>&1; then fail "a prices.refresh.maxChangePercent above 25 must not render"; fi
if render --set prices.refresh.maxChangePercent=0 >/dev/null 2>&1; then fail "a prices.refresh.maxChangePercent below 1 must not render"; fi
if render --set prices.refresh.maxAutoPerDay=11 >/dev/null 2>&1; then fail "a prices.refresh.maxAutoPerDay above 10 must not render"; fi
if render --set prices.refresh.maxAutoPerDay=0 >/dev/null 2>&1; then fail "a prices.refresh.maxAutoPerDay below 1 must not render"; fi

# NOTES are printed by install, not by template; each mode says what it does.
notes() { "$HELM" install koinon "$CHART" -n koinon --dry-run=client --set openshell.insecure=true --set "$ADDRESS" "$@" 2>&1 || true; }
notes_auto=$(notes)
grep -q 'prices.refresh is auto' <<<"$notes_auto" || fail "NOTES do not name the auto mode by default"
grep -q 'The first day brings no OpenRouter estimate' <<<"$notes_auto" || fail "NOTES do not say the first day has no OpenRouter estimate"
notes_off=$(notes --set prices.refresh.enabled=false)
grep -q 'prices.refresh is off' <<<"$notes_off" || fail "no NOTES line when the price refresh is off"
notes_on=$(notes --set prices.refresh.enabled=true)
grep -q 'prices.refresh is on' <<<"$notes_on" || fail "NOTES do not name the on mode"
if grep -q 'prices.refresh is off' <<<"$notes_auto$notes_on"; then fail "NOTES say the refresh is off when it is not"; fi
