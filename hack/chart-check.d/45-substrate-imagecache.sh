# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the in-cluster image cache of the Substrate driver (issue #1170, ADR 0120 section 6.9): a Zot
# pull-through registry that is the ONE image source of atelet. Sourced in order by hack/chart-check.sh; it uses that
# script's helpers (render, has, lacks, ...). The operator image source (imageCache.enabled=false) is in
# 44-substrate-worker.sh.

isub=(--set execution.driver=substrate --set gateway.model.driver=koinon --set gateway.mcp.driver=koinon --set execution.substrate.controlAddress=api.ate-system.svc:443
	--set execution.substrate.routerAddress=atenet-router.ate-system.svc:443 --set execution.substrate.tokenAudience=ateapi
	--set execution.substrate.allowUnhardened=true --set execution.substrate.defaultTier=dev
	--set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --set sandboxLimitRange.enabled=false)
icache=(--show-only templates/substrate-imagecache.yaml)
inp=(--show-only templates/substrate-networkpolicy.yaml)
icfg=(--show-only templates/config.yaml)
iens=(--show-only templates/ensure-secrets.yaml)
# idoc <awk regex>: the YAML documents of stdin that match the regex (documents split on ---).
idoc() { awk -v pat="$1" '/^---$/{ if (buf ~ pat) printf "%s\n---\n", buf; buf=""; next } { buf = buf $0 "\n" } END { if (buf ~ pat) printf "%s", buf }'; }

# Not with the OpenShell driver.
lacks "no image cache with the openshell driver" 'imagecache'
lacks "no image cache with the openshell driver (the zot image)" 'project-zot'

# The default render has the cache.
icdocs=$(render "${isub[@]}" "${icache[@]}")
for k in ServiceAccount ConfigMap Deployment Service NetworkPolicy; do
	grep -Eq "^kind: $k\$" <<<"$icdocs" || fail "the image cache must render a $k"
done
lacks "no PersistentVolumeClaim without persistence" 'kind: PersistentVolumeClaim' "${isub[@]}" "${icache[@]}"
idep=$(idoc '\nkind: Deployment\n' <<<"$icdocs")
grep -Fq 'name: koinon-imagecache' <<<"$idep" || fail "the cache Deployment is named <fullname>-imagecache"
grep -Eq 'image: ghcr.io/project-zot/zot:v[0-9.]+@sha256:[a-f0-9]{64}' <<<"$idep" || fail "the Zot image must be pinned by digest"
grep -Fq 'automountServiceAccountToken: false' <<<"$idep" || fail "the cache pod must have no ServiceAccount token"
grep -Fq 'type: Recreate' <<<"$idep" || fail "the cache has one store: the old pod stops first"
grep -Fq 'readOnlyRootFilesystem: true' <<<"$idep" || fail "the cache pod must have a read-only root"
grep -Fq 'runAsNonRoot: true' <<<"$idep" || fail "the cache pod must run as non-root"
grep -Fq 'scheme: HTTPS' <<<"$idep" || fail "the cache probes must use TLS"
grep -Fq 'secretName: koinon-imagecache-tls' <<<"$idep" || fail "the cache must mount the generated TLS Secret"
# Zot reloads its server certificate when the mounted files change (issue #1191, point 3). The kubelet updates a Secret
# volume only without subPath, so a subPath here would stop a rotated leaf certificate from reaching the cache.
grep -A3 -- '- name: tls$' <<<"$idep" | hit -F 'subPath' && fail "the cache pod must mount its TLS Secret without subPath (Zot reloads the certificate)"
grep -Fq 'sizeLimit: 20Gi' <<<"$idep" || fail "the cache store must be bounded without persistence"
# Every image through koinon.imageRef, every pod with the pull secrets (ADR 0061).
has "the Zot image follows global.registryMirrors" 'image: mirror.example.com/ghcr/project-zot/zot:' "${isub[@]}" --set 'global.registryMirrors.ghcr\.io=mirror.example.com/ghcr' "${icache[@]}"
has "the Zot image follows global.imageRegistry" 'image: mirror.example.com/all/project-zot/zot:' "${isub[@]}" --set global.imageRegistry=mirror.example.com/all "${icache[@]}"
has "the cache pod has the pull secrets" 'name: koinon-pull' "${isub[@]}" --set 'global.imagePullSecrets={koinon-pull}' "${icache[@]}"

# Zot config: sync on demand for the listed upstreams only; anonymous read and no more; TLS.
icm=$(idoc '\nkind: ConfigMap\n' <<<"$icdocs")
for h in ghcr.io docker.io quay.io; do
	grep -Fq "\"destination\": \"/$h\"" <<<"$icm" || fail "the Zot sync config must have an entry for $h"
done
grep -Fq '"https://registry-1.docker.io"' <<<"$icm" || fail "docker.io must sync from registry-1.docker.io"
grep -Fq '"stripPrefix": true' <<<"$icm" || fail "the sync entries must strip the host prefix"
if grep -Eq '"onDemand": false|pollInterval|"onlySigned"' <<<"$icm"; then fail "the cache syncs on demand only"; fi
grep -Fq '"anonymousPolicy"' <<<"$icm" || fail "the cache must have an access policy"
grep -Eq '"anonymousPolicy": \[\s*"read"\s*\]' <<<"$(tr -d '\n' <<<"$icm")" || fail "the cache allows anonymous read and nothing else"
grep -Eq '"compat": \[\s*"docker2s2"\s*\]' <<<"$(tr -d '\n' <<<"$icm")" || fail "Zot needs http.compat docker2s2 for Docker media types"
grep -Fq '"cert": "/etc/zot/tls/tls.crt"' <<<"$icm" || fail "the cache must serve TLS"
grep -Fq '"tlsVerify": true' <<<"$icm" || fail "the cache must verify the upstream certificates"
has "an upstream is configurable" '"destination": "/registry.example.com"' "${isub[@]}" --set 'execution.substrate.imageCache.upstreams[0].host=registry.example.com' --set 'execution.substrate.imageCache.upstreams[0].url=https://registry.example.com' "${icache[@]}"
if render "${isub[@]}" --set 'execution.substrate.imageCache.upstreams[0].host=r.example.com' --set 'execution.substrate.imageCache.upstreams[0].url=http://r.example.com' >/dev/null 2>&1; then fail "an upstream must be https"; fi

# NetworkPolicy of the cache: ingress from atelet only, egress to DNS and 443 outside the private ranges.
inpdoc=$(idoc '\nkind: NetworkPolicy\n' <<<"$icdocs")
grep -Fq 'kubernetes.io/metadata.name: "ate-system"' <<<"$inpdoc" || fail "the cache accepts ingress from the control-plane namespace"
grep -Fq -- '- atelet' <<<"$inpdoc" || fail "the cache accepts ingress from atelet only"
grep -Fq '{port: 443, protocol: TCP}' <<<"$inpdoc" || fail "the cache egress is TCP 443"
grep -Fq -- '- "10.0.0.0/8"' <<<"$inpdoc" || fail "the cache must not reach the private ranges"
has "the egress CIDRs can be fenced" 'cidr: "198.51.100.0/24"' "${isub[@]}" --set 'execution.substrate.imageCache.egress.cidrs={198.51.100.0/24}' "${icache[@]}"
lacks "fenced egress has no open internet rule" 'cidr: 0.0.0.0/0' "${isub[@]}" --set 'execution.substrate.imageCache.egress.cidrs={198.51.100.0/24}' "${icache[@]}"

# atelet's one image source is the cache pod, in Koinon's namespace, at the cache port.
isrc=$(idoc 'name: koinon-substrate-image-source\n' <<<"$(render "${isub[@]}" "${inp[@]}")")
grep -Fq 'app.kubernetes.io/component: imagecache' <<<"$isrc" || fail "the image-source policy must select the cache pods"
grep -Fq 'kubernetes.io/metadata.name: koinon' <<<"$isrc" || fail "the image-source policy must name Koinon's namespace"
grep -Fq '{port: 5000, protocol: TCP}' <<<"$isrc" || fail "the image-source policy must use the cache port"
has "the cache port is configurable" '\{port: 5443, protocol: TCP\}' "${isub[@]}" --set execution.substrate.imageCache.port=5443 "${inp[@]}"
# One image source: the cache and an operator source together fail.
if render "${isub[@]}" --set 'execution.substrate.workerNetworkPolicy.imageSource.cidrs={198.51.100.0/24}' >/dev/null 2>"$out/ic.err"; then fail "the cache and an operator CIDR are two image sources"; fi
grep -Fq 'imageCache.enabled' "$out/ic.err" || fail "the two-sources error must name imageCache.enabled"
if render "${isub[@]}" --set 'execution.substrate.imageMirrors.ghcr\.io=m.example.com/g' >/dev/null 2>&1; then fail "the cache and an operator mirror are two image sources"; fi
if render "${isub[@]}" --set execution.substrate.imageRegistry=m.example.com >/dev/null 2>&1; then fail "the cache and an operator registry are two image sources"; fi

# The driver rewrites the actor images to the cache: one entry per upstream, with the digest kept by the driver.
icfgd=$(render "${isub[@]}" "${icfg[@]}")
for h in ghcr.io docker.io quay.io; do
	grep -Fq -- "- \"$h=koinon-imagecache.koinon.svc:5000/$h\"" <<<"$icfgd" || fail "the config must map $h to the cache"
done
has "the mirror name follows the release namespace and the port" '"ghcr.io=koinon-imagecache.koinon.svc:5443/ghcr.io"' "${isub[@]}" --set execution.substrate.imageCache.port=5443 "${icfg[@]}"
lacks "no operator registry with the cache" 'imageRegistry:' "${isub[@]}" "${icfg[@]}"
# Deterministic: two renders are equal (no lookup, no random value).
[ "$(render "${isub[@]}" "${icache[@]}")" == "$icdocs" ] || fail "the cache render must be deterministic"

# TLS: the hook Job makes the Secret for the DNS names of the Service, or the operator's Secret is used.
iens_out=$(render "${isub[@]}" "${iens[@]}")
grep -Fq 'kind\":\"tls' <<<"$iens_out" || fail "the ensure-secrets Job must make the cache TLS Secret"
for n in koinon-imagecache koinon-imagecache.koinon koinon-imagecache.koinon.svc koinon-imagecache.koinon.svc.cluster.local; do
	grep -Fq "$n" <<<"$iens_out" || fail "the cache certificate must name $n"
done
has "the cluster domain is configurable" 'koinon-imagecache.koinon.svc.example.internal' "${isub[@]}" --set execution.substrate.imageCache.clusterDomain=example.internal "${iens[@]}"
lacks "an existing TLS Secret is not generated" 'koinon-imagecache-tls' "${isub[@]}" --set execution.substrate.imageCache.tls.existingSecret=my-tls "${iens[@]}"
has "an existing TLS Secret is mounted" 'secretName: my-tls' "${isub[@]}" --set execution.substrate.imageCache.tls.existingSecret=my-tls "${icache[@]}"

# Persistence.
has "persistence renders a PVC" 'kind: PersistentVolumeClaim' "${isub[@]}" --set execution.substrate.imageCache.persistence.enabled=true "${icache[@]}"
has "persistence mounts the PVC" 'claimName: koinon-imagecache' "${isub[@]}" --set execution.substrate.imageCache.persistence.enabled=true "${icache[@]}"

# Disabled: no cache objects, no cache TLS Secret, and the operator mirror reaches the driver config.
lacks "no cache objects when disabled" 'imagecache' "${isub[@]}" --set execution.substrate.imageCache.enabled=false
has "an operator mirror reaches the driver" '"ghcr.io=m.example.com/ghcr"' "${isub[@]}" --set execution.substrate.imageCache.enabled=false --set 'execution.substrate.imageMirrors.ghcr\.io=m.example.com/ghcr' "${icfg[@]}"
has "an operator registry reaches the driver" 'imageRegistry: "m.example.com/all"' "${isub[@]}" --set execution.substrate.imageCache.enabled=false --set execution.substrate.imageRegistry=m.example.com/all "${icfg[@]}"
lacks "no cache TLS Secret when disabled" 'imagecache-tls' "${isub[@]}" --set execution.substrate.imageCache.enabled=false "${iens[@]}"
lacks "no Kyverno" '[Kk]yverno' "${isub[@]}" "${icache[@]}"

# An IPv4-only except list renders no empty IPv6 except; a mixed list splits by family.
has "an IPv6 except stays under ::/0" '"fc00::/7"' "${isub[@]}" --set 'execution.substrate.imageCache.egress.exceptCIDRs={10.0.0.0/8,fc00::/7}' "${icache[@]}"

# atelet trusts the cache CA (issue #1191). The installer patches the atelet DaemonSet on every route
# (internal/substrateinstall, issue #1194); this hook Job fills the ConfigMap it mounts with the PUBLIC CA, and rolls atelet and the cache when the certificates change.
icaf=(--show-only templates/substrate-imagecache-ca.yaml)
lacks "no CA publishing without the cache" 'imagecache-ca' "${isub[@]}" --set execution.substrate.imageCache.enabled=false
icadocs=$(render "${isub[@]}" "${icaf[@]}")
icajob=$(idoc '\nkind: Job\n' <<<"$icadocs")
grep -Fq 'helm.sh/hook: pre-install,pre-upgrade' <<<"$icajob" || fail "the CA Job is a pre-install and pre-upgrade hook"
grep -Fq 'helm.sh/hook-weight: "-14"' <<<"$icajob" || fail "the CA Job runs after ensure-secrets (weight -15), which makes the TLS Secret"
grep -Fq -- '- "--tls-secret=koinon-imagecache-tls"' <<<"$icajob" || fail "the CA Job reads the generated TLS Secret"
grep -Fq -- '- "--substrate-namespace=ate-system"' <<<"$icajob" || fail "the CA Job writes to the Substrate namespace"
grep -Fq -- '- "--cache-deployment=koinon-imagecache"' <<<"$icajob" || fail "the CA Job stamps the cache Deployment"
grep -Fq 'readOnlyRootFilesystem: true' <<<"$icajob" || fail "the CA Job must have a read-only root"
grep -Fq 'runAsNonRoot: true' <<<"$icajob" || fail "the CA Job must run as non-root"
has "the CA Job reads an existing TLS Secret" '--tls-secret=my-tls' "${isub[@]}" --set execution.substrate.imageCache.tls.existingSecret=my-tls "${icaf[@]}"
has "the CA Job follows the Substrate namespace" '--substrate-namespace=ate-other' "${isub[@]}" --set execution.substrate.namespace=ate-other --set execution.substrate.gatewayClientNamespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=ate-other "${icaf[@]}"
has "the CA Job has the pull secrets" 'name: koinon-pull' "${isub[@]}" --set 'global.imagePullSecrets={koinon-pull}' "${icaf[@]}"
# Admission: the hook Job may stamp the atelet pod template and nothing else (policy koinon-koinon-worker-objects). The rule
# exists only with the cache, and the Job is not an installer identity.
ipol=(--show-only templates/substrate-policy.yaml)
ipdocs=$(render "${isub[@]}" "${ipol[@]}" | tr -d '\134')
grep -Fq "request.userInfo.username == \"system:serviceaccount:koinon:koinon-imagecache-ca\"" <<<"$ipdocs" || fail "the stamp rule names the CA hook ServiceAccount of the release"
grep -Fq "variables.installer || variables.kube || variables.controllerOwn || variables.controllerMetadata || variables.caStamp" <<<"$ipdocs" || fail "the objects policy admits the CA stamp"
grep -Fq "object.spec.template.spec == oldObject.spec.template.spec" <<<"$ipdocs" || fail "the stamp rule keeps the pod spec unchanged"
lacks "no CA stamp rule without the cache" 'caStamp' "${isub[@]}" --set execution.substrate.imageCache.enabled=false "${ipol[@]}"
# RBAC: the Secret is read by name and never written; the key of the Secret has no route to the Substrate namespace.
icarole=$(idoc '\nkind: Role\nmetadata:\n  name: koinon-imagecache-ca\n  labels' <<<"$icadocs")
grep -Fq 'resourceNames: ["koinon-imagecache-tls"]' <<<"$icarole" || fail "the release Role reads the TLS Secret by name"
if grep -Eq 'verbs: \[.*(update|create|delete|patch).*\]' <<<"$(awk '/resources: \[secrets\]/{f=1} f&&/verbs/{print; f=0}' <<<"$icarole")"; then fail "the CA Job must never write a Secret"; fi
icasub=$(awk 'BEGIN{RS="---\n"} /kind: Role\n/ && /namespace: ate-system/ {print}' <<<"$icadocs")
grep -Fq 'resourceNames: ["koinon-imagecache-ca"]' <<<"$icasub" || fail "the Substrate Role changes the one ConfigMap by name"
grep -Fq 'resourceNames: ["atelet"]' <<<"$icasub" || fail "the Substrate Role patches the atelet DaemonSet by name"
if grep -Eq 'resources: \[(secrets|pods|roles|rolebindings|serviceaccounts)' <<<"$icasub"; then fail "the Substrate Role must not reach Secrets, pods or RBAC"; fi
lacks "the CA Job renders no key material" 'tls\.key|PRIVATE KEY' "${isub[@]}" "${icaf[@]}"
lacks "no Kyverno in the CA Job" '[Kk]yverno' "${isub[@]}" "${icaf[@]}"
[ "$(render "${isub[@]}" "${icaf[@]}")" == "$icadocs" ] || fail "the CA Job render must be deterministic"

# egress.extra (issue #1091): off by default; one rule for each entry. The default render keeps today's egress: DNS and 443.
ic_default=$(render "${isub[@]}" "${icache[@]}")
ic_extra=$(render "${isub[@]}" "${icache[@]}" --set 'execution.substrate.imageCache.egress.extra[0].cidr=172.18.0.0/16' --set 'execution.substrate.imageCache.egress.extra[0].port=5000')
[ "$(grep -c "protocol: TCP" <<<"$ic_extra")" -eq "$(( $(grep -c "protocol: TCP" <<<"$ic_default") + 1 ))" ] || fail "one egress.extra entry adds exactly one TCP port line, and the default adds none"
[ "$(grep -c 'ipBlock:' <<<"$ic_extra")" -eq "$(( $(grep -c 'ipBlock:' <<<"$ic_default") + 1 ))" ] || fail "one extra ipBlock must come from one egress.extra entry"
grep -Fq 'cidr: "172.18.0.0/16"' <<<"$ic_extra" || fail "egress.extra must name its CIDR"
grep -Eq 'port: 5000, protocol: TCP' <<<"$ic_extra" || fail "egress.extra must name its port"
grep -Eq 'port: 443, protocol: TCP' <<<"$ic_extra" || fail "egress.extra must keep the 443 rule"
capacity_refused "an egress.extra entry without a port" "port" "${isub[@]}" "${icache[@]}" --set 'execution.substrate.imageCache.egress.extra[0].cidr=172.18.0.0/16'
has "an http upstream renders with allowHTTP" '"destination": "/r.example.com"' "${isub[@]}" --set 'execution.substrate.imageCache.upstreams[0].host=r.example.com' --set 'execution.substrate.imageCache.upstreams[0].url=http://r.example.com:5000' --set 'execution.substrate.imageCache.upstreams[0].allowHTTP=true' "${icache[@]}"
# A host with a port (issue #1091): the mirror key is the real host, and the path segment after the cache host and the Zot
# destination have no colon (ateapi and atelet refuse a colon in a repository path).
pp=(--set 'execution.substrate.imageCache.upstreams[0].host=localhost:5001' --set 'execution.substrate.imageCache.upstreams[0].url=http://kind-registry:5000' --set 'execution.substrate.imageCache.upstreams[0].allowHTTP=true')
has "the mirror key of a host with a port is the real host" '"localhost:5001=koinon-imagecache\.koinon\.svc:[0-9]+/localhost_5001"' "${isub[@]}" "${pp[@]}" "${icfg[@]}"
lacks "no colon in the mirror path of a host with a port" 'koinon\.svc:[0-9]+/localhost:' "${isub[@]}" "${pp[@]}" "${icfg[@]}"
has "the Zot destination of a host with a port has no colon" '"destination": "/localhost_5001"' "${isub[@]}" "${pp[@]}" "${icache[@]}"
