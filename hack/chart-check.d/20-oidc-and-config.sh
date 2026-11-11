# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: oidc values, config.yaml, the gateway admin listener.
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# oidc values map to koinon-api's --oidc-* flags; the client secret is a mounted file.
oidc=(--set publicUrl=https://koinon.example.com --set oidc.enabled=true --set oidc.issuerUrl=https://idp.example.com
	--set oidc.clientId=koinon --set oidc.clientSecret.existingSecret=koinon-oidc --set oidc.groupMappings.staff=member)
lacks "no OIDC env when oidc is disabled" 'KOINON_OIDC_'
env_is KOINON_OIDC_ISSUER_URL '"https://idp.example.com"' "${oidc[@]}"
env_is KOINON_OIDC_CLIENT_ID '"koinon"' "${oidc[@]}"
env_is KOINON_OIDC_CLIENT_SECRET_FILE /etc/koinon/oidc/client-secret "${oidc[@]}"
env_is KOINON_OIDC_SCOPES '"email,profile"' "${oidc[@]}"
env_is KOINON_DEFAULT_ORG_GROUP_MAPPINGS '"staff=member"' "${oidc[@]}"
env_is KOINON_SESSION_MAX_LIFETIME '"12h"' "${oidc[@]}"
has "OIDC client secret mounted" 'secretName: koinon-oidc$' "${oidc[@]}"
if render --set oidc.enabled=true --set oidc.issuerUrl=https://idp.example.com --set oidc.clientId=koinon \
	>/dev/null 2>&1; then
	fail "oidc without a redirect or public URL must not render"
fi
# config.yaml is read by the reconciler and by the API (its sandboxResources section, ADR 0083: the API
# refuses a per-agent override outside the bounds), not by koinon-agentd.
n=$(render --show-only templates/deployments.yaml | grep -c 'KOINON_CONFIG' || true)
[ "$n" = 2 ] || fail "KOINON_CONFIG set on $n components, want the reconciler and the API"
n=$(render --show-only templates/deployments.yaml | grep -c 'mountPath: /etc/koinon/config$' || true)
[ "$n" = 2 ] || fail "config.yaml mounted into $n components, want the reconciler and the API"
n=$(render --show-only templates/deployments.yaml | grep -c 'checksum/config:' || true)
[ "$n" = 2 ] || fail "checksum/config on $n components, want the reconciler and the API (they roll when the file changes)"
# The API reads only sandboxResources: its checksum ignores a reconciler-only value, follows a sandbox
# resources value, and the reconciler's checksum follows both.
sum_of() { # <component> [helm args]: the checksum/config annotation of one component
	local c=$1
	shift
	render "$@" --show-only templates/deployments.yaml | awk -v c="koinon-$c" '$0 ~ "name: .*" c "$" {f=1} f && /checksum\/config:/ {print $2; exit}'
}
api0=$(sum_of api)
rec0=$(sum_of reconciler)
[ -n "$api0" ] && [ -n "$rec0" ] || fail "checksum/config not found on the API or the reconciler"
[ "$(sum_of api --set reconciler.resyncInterval=7m)" = "$api0" ] || fail "a reconciler-only value changed the API checksum/config"
[ "$(sum_of reconciler --set reconciler.resyncInterval=7m)" != "$rec0" ] || fail "a reconciler value left the reconciler checksum/config unchanged"
[ "$(sum_of api --set sandbox.resources.default.cpu.limit=3)" != "$api0" ] || fail "a sandbox.resources value left the API checksum/config unchanged"
# agentgateway's admin listener (config dump and UI) binds to localhost: no Service or container
# port exposes it (the config the reconciler renders keeps it on localhost: internal/gateway/standalone tests).
for f in "${value_files[@]}"; do
	if grep -Eq -- '(containerPort|port|targetPort): 15000$' "$out/$(basename "$f" .yaml).yaml"; then
		fail "port 15000 exposed with $(basename "$f")"
	fi
done
