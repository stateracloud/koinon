# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: api.trustedProxies, the proxies whose X-Forwarded-For the login and admin API
# limiters believe (issue 859). Sourced in order by hack/chart-check.sh; it uses that script's helpers.

lacks "no trusted proxies by default: the flag is not passed" 'trusted-proxies'
lacks "an empty list passes no flag" 'trusted-proxies' --set-json 'api.trustedProxies=[]'
has "one CIDR is passed" '"--trusted-proxies", "10.0.0.0/8"' --set 'api.trustedProxies={10.0.0.0/8}'
has "several are passed comma-separated" '"--trusted-proxies", "10.0.0.0/8,fd00::/8"' --set 'api.trustedProxies={10.0.0.0/8,fd00::/8}'
if render --set 'api.trustedProxies={""}' >/dev/null 2>&1; then fail "an empty api.trustedProxies entry must not render"; fi
if render --set api.trustedProxies=10.0.0.0/8 >/dev/null 2>&1; then fail "api.trustedProxies takes a list, not a string"; fi
