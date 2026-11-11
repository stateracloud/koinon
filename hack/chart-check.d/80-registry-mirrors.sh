# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: registry mirrors and pull secrets.
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# Registry mirrors and pull secrets (old issue 130): global.imageRegistry, global.registryMirrors and
# global.imagePullSecrets reach every image the chart runs or names. image_refs lists every image
# reference (image:, imageName:, --probe-image, the supervisor pin) of a render, sorted and unique.
image_refs() {
	render "$@" | grep -E -- '^ *(- )?(image|imageName): |--probe-image=|--supervisor-images=|c\.image in ' |
		grep -Eo -- '([a-z0-9.-]+(:[0-9]+)?/)+[A-Za-z0-9._/-]+:[A-Za-z0-9._-]+(@sha256:[a-f0-9]{64})?' | sort -u
}
agw=ghcr.io/agentgateway/agentgateway:v1.7.0@sha256:60216aafb03d37f94e952ff3d51d2f630a65b10ecaebdc0a864882f92610c45b
pg=ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie@sha256:f7e00138159bb45d70b624ba25e446924cb3ed1bf366d13e936c3f81b914064e
# (a) nothing set: every reference is the upstream one, unchanged.
want=$(printf '%s\n' "$agw" "$pg" ghcr.io/nvidia/openshell/supervisor:0.1.7 ghcr.io/stateracloud/koinon/flavors/opencode:0.0.0 \
	ghcr.io/stateracloud/koinon/agentd:0.0.0 ghcr.io/stateracloud/koinon/api:0.0.0 ghcr.io/stateracloud/koinon/reconciler:0.0.0 | sort -u)
[[ $(image_refs) == "$want" ]] || fail "default image references changed: $(image_refs | tr '\n' ' ')"
# The public repository's packages (#167): none of the old private-repository names may come back.
image_refs | grep -Eq -- 'stateracloud/(koinon-|flavor-opencode)' && fail "an old package name is rendered: $(image_refs | tr '\n' ' ')"
# (b) imageRegistry replaces every host and keeps path, tag and digest.
want=$(printf '%s\n' "art.example.com/proxy/${agw#ghcr.io/}" "art.example.com/proxy/${pg#ghcr.io/}" art.example.com/proxy/nvidia/openshell/supervisor:0.1.7 \
	art.example.com/proxy/stateracloud/koinon/flavors/opencode:0.0.0 art.example.com/proxy/stateracloud/koinon/agentd:0.0.0 \
	art.example.com/proxy/stateracloud/koinon/api:0.0.0 art.example.com/proxy/stateracloud/koinon/reconciler:0.0.0 | sort -u)
[[ $(image_refs --set global.imageRegistry=art.example.com/proxy) == "$want" ]] ||
	fail "global.imageRegistry did not rewrite every image: $(image_refs --set global.imageRegistry=art.example.com/proxy | tr '\n' ' ')"
# (c) the map wins for its host; imageRegistry rewrites the others (the CNPG image is on another upstream here).
mixed=(--set global.imageRegistry=art.example.com/proxy --set 'global.registryMirrors.ghcr\.io=art.example.com/ghcr-remote'
	--set postgres.cnpg.imageName=quay.io/enterprisedb/postgresql:18@sha256:f7e00138159bb45d70b624ba25e446924cb3ed1bf366d13e936c3f81b914064e)
refs=$(image_refs "${mixed[@]}")
grep -Fxq -- "art.example.com/ghcr-remote/stateracloud/koinon/api:0.0.0" <<<"$refs" || fail "registryMirrors[ghcr.io] must rewrite Koinon's images: $refs"
grep -Fxq -- "art.example.com/ghcr-remote/${agw#ghcr.io/}" <<<"$refs" || fail "registryMirrors[ghcr.io] must rewrite agentgateway, digest kept: $refs"
grep -Fxq -- "art.example.com/proxy/enterprisedb/postgresql:18@sha256:f7e00138159bb45d70b624ba25e446924cb3ed1bf366d13e936c3f81b914064e" <<<"$refs" || fail "imageRegistry must rewrite hosts the map lacks: $refs"
grep -Fq -- "ghcr.io/" <<<"$refs" && fail "a ghcr.io reference survived the mirror: $refs"
# docker.io: no host, and a bare name is docker.io/library/name.
has "bare docker.io names go through the map" 'imageName: art.example.com/docker-remote/library/postgres:18$' \
	--set 'global.registryMirrors.docker\.io=art.example.com/docker-remote' --set postgres.cnpg.imageName=postgres:18
has "user/name is on docker.io" 'imageName: art.example.com/docker-remote/cnpg/postgres:18$' \
	--set 'global.registryMirrors.docker\.io=art.example.com/docker-remote' --set postgres.cnpg.imageName=cnpg/postgres:18
has "a registry with a port is a host" 'imageName: art.example.com/p/img:1$' \
	--set global.imageRegistry=art.example.com/p --set postgres.cnpg.imageName=registry.local:5000/img:1
# A reference already under the mirror is not prefixed again, whichever setting names the mirror.
has "a reference under another mirror value stays" 'imageName: art.example.com/ghcr-remote/org/x:1$' --set global.imageRegistry=proxy.example.com/p \
	--set 'global.registryMirrors.ghcr\.io=art.example.com/ghcr-remote' --set postgres.cnpg.imageName=art.example.com/ghcr-remote/org/x:1
has "image.registry already under imageRegistry does not double" 'image: art.example.com/ghcr-remote/stateracloud/koinon/api:0.0.0$' \
	--set image.registry=art.example.com/ghcr-remote/stateracloud/koinon --set global.imageRegistry=art.example.com/ghcr-remote
# Docker Hub's other names and upper case are docker.io.
for h in index.docker.io registry-1.docker.io Docker.IO; do
	has "$h is docker.io" 'imageName: art.example.com/docker-remote/library/postgres:18$' \
		--set 'global.registryMirrors.docker\.io=art.example.com/docker-remote' --set "postgres.cnpg.imageName=$h/library/postgres:18"
done
# A URL is not a registry.
if render --set global.imageRegistry=https://art.example.com >/dev/null 2>&1; then fail "imageRegistry with a scheme must not render"; fi
if render --set 'global.registryMirrors.ghcr\.io=https://art.example.com' >/dev/null 2>&1; then fail "a mirror with a scheme must not render"; fi
# NOTES warn when image.registry is customised next to a global mirror (helm install prints them; template does not).
# Do not hide a helm failure with || true: that turned an unsupported helm into "missing NOTES" (#349).
notes=""
if ! notes=$("$HELM" install koinon "$CHART" -n koinon --dry-run=client --set openshell.insecure=true --set "$ADDRESS" --set image.registry=quay.io/m --set global.imageRegistry=art.example.com 2>&1); then
	fail "helm install --dry-run=client for NOTES failed: $(head -1 <<<"$notes")"
else
	grep -q 'WARNING: image.registry is quay.io/m' <<<"$notes" || fail "no NOTES warning for image.registry with a global mirror"
fi
# An explicit image.registry is the upstream that the rewrite starts from.
has "image.registry is rewritten too" 'image: art.example.com/ghcr-remote/mine/api:0.0.0$' \
	--set image.registry=quay.io/mine --set 'global.registryMirrors.quay\.io=art.example.com/ghcr-remote'
has "an explicit image.registry stands without a mirror" 'image: quay.io/mine/api:0.0.0$' --set image.registry=quay.io/mine
# (e) the default supervisor pin follows the mirror, so the admission policy admits the rewritten image; an explicit list is used as written.
has "supervisor pin is rewritten" 'c.image in \["art.example.com/ghcr-remote/nvidia/openshell/supervisor:0.1.7"\]' \
	--set 'global.registryMirrors.ghcr\.io=art.example.com/ghcr-remote'
has "explicit supervisorImages are not rewritten" 'c.image in \["ghcr.io/nvidia/openshell/supervisor:0.1.6"\]' \
	--set global.imageRegistry=art.example.com/proxy --set 'sandboxPolicy.supervisorImages={ghcr.io/nvidia/openshell/supervisor:0.1.6}'
has "preflight is told the supervisor pin" '--supervisor-images=art.example.com/proxy/nvidia/openshell/supervisor:0.1.7' --set global.imageRegistry=art.example.com/proxy
lacks "no supervisor pin to check when the policy is off" '--supervisor-images' --set sandboxPolicy.enabled=false --set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false
# (d) pull secrets: strings and {name} maps, merged with the deprecated top-level list, on every pod-bearing object.
pull_args=(--set 'global.imagePullSecrets[0]=regcred' --set 'global.imagePullSecrets[1].name=other' --set 'imagePullSecrets[0].name=regcred')
rendered=$(render "${pull_args[@]}")
pods=$(grep -cE '^ *(containers|initContainers):' <<<"$rendered" || true)
withsecrets=$(grep -cE '^ +imagePullSecrets:$' <<<"$rendered" || true)
# Deployments (api, reconciler, agentd, gateway), the migrate, migrate-grants (cnpg mode, ADR 0100),
# ensure-secrets, preflight and restore-release-check Jobs: 9 pod specs; the CNPG Cluster has its own imagePullSecrets (spec level).
[[ $withsecrets == 10 ]] || fail "imagePullSecrets on $withsecrets objects, want 10 (9 pod specs and the CNPG Cluster); pod specs: $pods"
[[ $(grep -c -- '- name: regcred$' <<<"$rendered") == 10 ]] || fail "regcred must appear once per object, de-duplicated across the two lists"
has "pull secrets reach the preflight probe pods" '--probe-image-pull-secrets=regcred,other$' "${pull_args[@]}"
has "the deprecated top-level list still works alone" '--probe-image-pull-secrets=old$' --set 'imagePullSecrets[0].name=old'
lacks "no imagePullSecrets without any" 'imagePullSecrets:'
has "the preflight expects sandbox pull secrets with a pull secret" '--expect-sandbox-pull-secrets' --set 'global.imagePullSecrets[0]=x'
has "the preflight expects sandbox pull secrets with a mirror" '--expect-sandbox-pull-secrets' --set global.imageRegistry=art.example.com
lacks "the preflight expects none by default" '--expect-sandbox-pull-secrets'
has "the preflight may read OpenShell's gateway ConfigMap, by name" 'resourceNames: \[openshell-config\]'

