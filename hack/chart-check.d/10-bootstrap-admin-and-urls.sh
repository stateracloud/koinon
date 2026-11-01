# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: bootstrapAdmin, publicUrl, httpRoute.
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# bootstrapAdmin.enabled=false turns password login off and renders no password.
env_is KOINON_BOOTSTRAP_LOGIN '"false"' --set bootstrapAdmin.enabled=false
lacks "no bootstrap Secret when bootstrapAdmin is disabled" 'name: koinon-bootstrap-admin$' --set bootstrapAdmin.enabled=false
lacks "no password file when bootstrapAdmin is disabled" 'KOINON_BOOTSTRAP_PASSWORD_FILE' --set bootstrapAdmin.enabled=false
lacks "bootstrap login stays on by default" 'KOINON_BOOTSTRAP_LOGIN'
# bootstrapAdmin.username reaches koinon-api.
env_is KOINON_BOOTSTRAP_ADMIN_USERNAME '"admin"'
env_is KOINON_BOOTSTRAP_ADMIN_USERNAME '"ops"' --set bootstrapAdmin.username=ops
# bootstrapAdmin.existingSecret: no generated Secret, the API mounts the given one.
# The chart renders no password: the ensure-secrets hook Job creates the missing Secrets.
lacks "no generated bootstrap Secret with existingSecret" 'name: koinon-bootstrap-admin$' --set bootstrapAdmin.existingSecret=mine
has "existingSecret mounted" 'secretName: mine$' --set bootstrapAdmin.existingSecret=mine
# publicUrl: explicit, or derived from the ingress.
lacks "no public URL without ingress or publicUrl" 'KOINON_PUBLIC_URL'
env_is KOINON_PUBLIC_URL '"http://koinon.lan"' --set publicUrl=http://koinon.lan/
env_is KOINON_PUBLIC_URL '"http://koinon.example.com"' --set ingress.enabled=true --set ingress.host=koinon.example.com
env_is KOINON_PUBLIC_URL '"https://koinon.example.com"' --set ingress.enabled=true --set ingress.host=koinon.example.com \
	--set ingress.tlsSecretName=tls
# httpRoute: a Gateway API route with the given parentRefs (as written) and hostnames; the public URL
# derives from its first hostname over https.
route=(--set httpRoute.enabled=true --set 'httpRoute.hostnames[0]=koinon.example.com'
	--set-json 'httpRoute.parentRefs=[{"group":"gateway.networking.k8s.io","kind":"Gateway","name":"shared","namespace":"gateway","sectionName":"https"}]')
lacks "no HTTPRoute by default" 'kind: HTTPRoute'
has "HTTPRoute renders with httpRoute.enabled" '^kind: HTTPRoute$' "${route[@]}" --show-only templates/httproute.yaml
has "HTTPRoute keeps the parentRef's group and kind" 'group: gateway.networking.k8s.io' "${route[@]}" --show-only templates/httproute.yaml
has "HTTPRoute keeps the parentRef's sectionName" 'sectionName: https' "${route[@]}" --show-only templates/httproute.yaml
has "HTTPRoute routes to the API Service" 'name: koinon-api$' "${route[@]}" --show-only templates/httproute.yaml
env_is KOINON_PUBLIC_URL '"https://koinon.example.com"' "${route[@]}"
if render_raw --set openshell.insecure=true --set httpRoute.enabled=true >/dev/null 2>&1; then fail "httpRoute without hostnames and parentRefs must not render"; fi
if render "${route[@]}" --set ingress.enabled=true --set ingress.host=koinon.example.com >/dev/null 2>&1; then fail "ingress and httpRoute together must not render"; fi
if render --set httpRoute.enabled=true --set 'httpRoute.hostnames[0]=*.example.com' --set-json 'httpRoute.parentRefs=[{"name":"shared"}]' >/dev/null 2>&1; then fail "a wildcard first hostname without publicUrl must not render"; fi
has "httpRoute.timeouts reach the route's rule" 'backendRequest: 0s' "${route[@]}" --set httpRoute.timeouts.backendRequest=0s --show-only templates/httproute.yaml
