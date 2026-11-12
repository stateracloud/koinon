# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: api.trustedProxies, the proxies whose forwarding header the login and admin API
# limiters believe (issue 859). Sourced in order by hack/chart-check.sh; it uses that script's helpers.

lacks "no trusted proxies by default: no flag is passed" 'trusted-proxy'
lacks "an empty list passes no flag" 'trusted-proxy' --set-json 'api.trustedProxies=[]'
has "one CIDR is passed, with the default header" '"--trusted-proxies", "10.0.0.0/8", "--trusted-proxy-header", "x-forwarded-for"' --set 'api.trustedProxies={10.0.0.0/8}'
has "several are passed comma-separated" '"--trusted-proxies", "10.0.0.0/8,fd00::/8"' --set 'api.trustedProxies={10.0.0.0/8,fd00::/8}'
has "the header is passed" '"--trusted-proxy-header", "forwarded"' --set 'api.trustedProxies={10.0.0.0/8}' --set api.trustedProxyHeader=forwarded
if render --set-json 'api.trustedProxies=[""]' >/dev/null 2>&1; then fail "an empty api.trustedProxies entry must not render"; fi
if render --set-json 'api.trustedProxies=["a\", \"--durable"]' >/dev/null 2>&1; then fail "an api.trustedProxies entry that is not an address or CIDR must not render"; fi
if render --set api.trustedProxies=10.0.0.0/8 >/dev/null 2>&1; then fail "api.trustedProxies takes a list, not a string"; fi
if render --set api.trustedProxyHeader=x-real-ip >/dev/null 2>&1; then fail "api.trustedProxyHeader takes x-forwarded-for or forwarded"; fi
