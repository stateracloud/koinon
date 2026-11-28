# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the Substrate provisioner (ADR 0128, issue 1398). Sourced in order by hack/chart-check.sh; it
# uses that script's helpers (render, has, lacks, fail, ...). The provisioner is the one Koinon pod with a global
# role in Substrate; the reconciler and agentd hold none.

psub=(--set execution.driver=substrate --set gateway.model.driver=koinon --set gateway.mcp.driver=koinon --set execution.substrate.controlAddress=api.ate-system.svc:443
	--set execution.substrate.routerAddress=atenet-router.ate-system.svc:443 --set execution.substrate.tokenAudience=ateapi
	--set execution.substrate.allowUnhardened=true --set execution.substrate.defaultTier=dev
	--set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --set sandboxLimitRange.enabled=false
	--set execution.substrate.imageCache.enabled=false)
pfile=(--show-only templates/substrate-provisioner.yaml)

# Nothing of it renders with the default driver, or when it is turned off.
lacks "no provisioner with the openshell driver" 'koinon-provisioner'
lacks "no provisioner when it is off" 'koinon-provisioner' "${psub[@]}" --set execution.substrate.provisioner.enabled=false
lacks "the reconciler does not ask the provisioner when it is off" 'provisioner' "${psub[@]}" --set execution.substrate.provisioner.enabled=false --show-only templates/config.yaml

pdoc=$(render "${psub[@]}" "${pfile[@]}")
for want in 'kind: Deployment' 'kind: Service' 'kind: ServiceAccount' 'kind: ClusterRole$' 'kind: ClusterRoleBinding' 'kind: NetworkPolicy'; do
	grep -Eq -- "$want" <<<"$pdoc" || fail "the provisioner renders no $want"
done
# It runs the reconciler's image in its own mode, with the reconciler as its only caller and the reconciler and agentd as the grantees.
grep -Fq -- '- provision' <<<"$pdoc" || fail "the provisioner must run the provision mode"
grep -Fq -- '--caller=system:serviceaccount:koinon:koinon-reconciler' <<<"$pdoc" || fail "the reconciler must be the provisioner's only caller"
grep -Fq -- '--editor=user:system:serviceaccount:koinon:koinon-reconciler' <<<"$pdoc" || fail "the reconciler must get editor in each org atespace"
grep -Fq -- '--viewer=user:system:serviceaccount:koinon:koinon-agentd' <<<"$pdoc" || fail "agentd must get viewer in each org atespace"
if grep -Eq -- '--(editor|viewer|caller)=.*(provisioner|api)$' <<<"$pdoc"; then fail "only the reconciler and agentd are granted"; fi
# Its only cluster-scoped right is the TokenReview of the reconciler's token.
rules=$(awk '/^kind: ClusterRole$/{f=1} f&&/^---$/{f=0} f' <<<"$pdoc")
grep -Fq 'tokenreviews' <<<"$rules" || fail "the provisioner ClusterRole must create tokenreviews"
if grep -Eq 'secrets|pods|\*' <<<"$rules"; then fail "the provisioner ClusterRole must hold nothing but tokenreviews"; fi
# It holds no database login, key or guest key: the reconciler's other mounts are not its own.
if grep -Eq 'PGPASSWORD|DATABASE_URL|substrate-guest-key|kek' <<<"$pdoc"; then fail "the provisioner must hold no database, KEK or guest key"; fi
# Ingress only from the reconciler.
npdoc=$(awk '/^kind: NetworkPolicy$/{f=1} f' <<<"$pdoc")
grep -Fq 'app.kubernetes.io/component: reconciler' <<<"$npdoc" || fail "only reconciler pods may reach the provisioner"
# The reconciler gets the provisioner's address and a token with the provisioner's audience; agentd gets neither.
has "the reconciler's config names the provisioner" 'url: "http://koinon-provisioner.koinon.svc:8086"' "${psub[@]}" --show-only templates/config.yaml
recdoc=$(render "${psub[@]}" --show-only templates/deployments.yaml)
agentd=$(docof 'name: koinon-agentd\n' <<<"$recdoc")
if grep -Fq 'provisioner-token' <<<"$agentd"; then fail "agentd must not mount the provisioner token"; fi
grep -Fq 'provisioner-token' <<<"$(docof 'name: koinon-reconciler\n' <<<"$recdoc")" || fail "the reconciler must mount the provisioner token"
# A custom audience reaches both sides.
has "the provisioner audience is configurable" 'audience: "prov-x"' "${psub[@]}" --set execution.substrate.provisioner.tokenAudience=prov-x --show-only templates/deployments.yaml
has "the provisioner checks the configured audience" '--token-audience=prov-x' "${psub[@]}" --set execution.substrate.provisioner.tokenAudience=prov-x "${pfile[@]}"
