# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: execution.driver (ADR 0119, #1088). Sourced in order by hack/chart-check.sh; it
# uses that script's helpers (render, has, lacks, ...).

sub=(--set execution.driver=substrate --set execution.substrate.controlAddress=api.ate-system.svc:443
	--set execution.substrate.routerAddress=atenet-router.ate-system.svc:80 --set execution.substrate.tokenAudience=ateapi
	--set execution.substrate.allowUnhardened=true --set execution.substrate.defaultTier=dev
	--set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --set sandboxLimitRange.enabled=false)

# OpenShell stays the default: no Substrate input reaches any component.
has "the default driver is openshell" 'driver: openshell' --show-only templates/config.yaml
lacks "no Substrate volume with the default driver" 'substrate-token' --show-only templates/deployments.yaml
env_is KOINON_OPENSHELL_ADDRESS '"openshell.openshell.svc:8080"'

# Substrate: the reconciler config and agentd's env carry the addresses and the token file, and both
# components project the ServiceAccount token with the ateapi audience.
has "substrate driver in the reconciler config" 'driver: substrate' "${sub[@]}" --show-only templates/config.yaml
has "substrate control address in the reconciler config" 'controlAddress: "api.ate-system.svc:443"' "${sub[@]}" --show-only templates/config.yaml
has "substrate token file in the reconciler config" 'tokenFile: "/var/run/koinon/substrate/token"' "${sub[@]}" --show-only templates/config.yaml
env_is KOINON_EXECUTION_DRIVER substrate "${sub[@]}"
env_is KOINON_SUBSTRATE_ROUTER_ADDRESS '"atenet-router.ate-system.svc:80"' "${sub[@]}"
env_is KOINON_SUBSTRATE_TOKEN_FILE '"/var/run/koinon/substrate/token"' "${sub[@]}"
has "the ateapi token is a projected ServiceAccount token with the configured audience" 'audience: "ateapi"' "${sub[@]}" --show-only templates/deployments.yaml
lacks "agentd does not get the OpenShell address with substrate" 'KOINON_OPENSHELL_ADDRESS' "${sub[@]}" --show-only templates/deployments.yaml
has "the install preflight Job runs the Substrate checks with substrate (#1089; details in 95-preflight.sh)" '- --driver=substrate' "${sub[@]}" --show-only templates/preflight.yaml
has "the install preflight Job stays with openshell" 'component: preflight' 
has "a CA Secret is mounted and named" 'KOINON_SUBSTRATE_CA_FILE' "${sub[@]}" --set execution.substrate.caSecret=ate-ca --show-only templates/deployments.yaml
# openshell.address is not required with substrate, and the OpenShell-only features are refused.
"$HELM" template koinon "$CHART" --namespace koinon "${sub[@]}" >/dev/null 2>&1 || fail "substrate must render without openshell.address and TLS"
for flag in sandboxObserver sandboxNetworkPolicy sandboxLimitRange; do
	if render "${sub[@]}" --set "$flag.enabled=true" >/dev/null 2>"$out/sub.err"; then fail "$flag.enabled with substrate must not render"; fi
	grep -q "$flag.enabled" "$out/sub.err" || fail "the $flag error must name it"
done
if render --set execution.driver=substrate >/dev/null 2>"$out/sub.err"; then fail "substrate without addresses must not render"; fi
grep -q 'controlAddress' "$out/sub.err" || fail "the missing-address error must name execution.substrate.controlAddress"
if render --set execution.driver=nomad >/dev/null 2>&1; then fail "an unknown driver must not render"; fi
# Substrate is refused until the overlay image has a recorded run in a real actor (#1087).
if render "${sub[@]}" --set execution.substrate.allowUnhardened=false >/dev/null 2>"$out/sub.err"; then fail "substrate without allowUnhardened must not render"; fi
grep -q 'allowUnhardened' "$out/sub.err" || fail "the unhardened error must name execution.substrate.allowUnhardened"
env_is KOINON_SUBSTRATE_ALLOW_UNHARDENED '"true"' "${sub[@]}"

# The tiers reach both components: the driver starts no guest process without one (#1088).
env_is KOINON_SUBSTRATE_DEFAULT_TIER '"dev"' "${sub[@]}"
has "the default tier is in the reconciler config" 'defaultTier: "dev"' "${sub[@]}" --show-only templates/config.yaml
# kata=vm needs a microvm pool: the chart refuses the vm tier on the default gvisor pool (ADR 0075, 44-substrate-worker.sh).
tiers=(--set execution.substrate.runtimeClassTiers.kata=vm --set execution.substrate.runtimeClassTiers.gvisor=hardened --set execution.substrate.workerPool.sandboxClass=microvm)
env_is KOINON_SUBSTRATE_RUNTIME_CLASS_TIERS '"gvisor=hardened,kata=vm"' "${sub[@]}" "${tiers[@]}"
has "the class tiers are in the reconciler config" '- "kata=vm"' "${sub[@]}" "${tiers[@]}" --show-only templates/config.yaml
# The class the reconciler puts on every sandbox (openshell.runtimeClassName) always has a tier (#1245).
has "the requested class gets the default tier" '- "kata=dev"' "${sub[@]}" --show-only templates/config.yaml
env_is KOINON_SUBSTRATE_RUNTIME_CLASS_TIERS '"kata=dev"' "${sub[@]}"
env_is KOINON_SUBSTRATE_RUNTIME_CLASS_TIERS '"gvisor=hardened,kata=vm"' "${sub[@]}" "${tiers[@]}" --set execution.substrate.defaultTier=dev
env_is KOINON_SUBSTRATE_RUNTIME_CLASS_TIERS '"gvisor=hardened,kata=dev"' "${sub[@]}" --set execution.substrate.runtimeClassTiers.gvisor=hardened
if render "${sub[@]}" --set execution.substrate.defaultTier= --set execution.substrate.runtimeClassTiers.gvisor=hardened >/dev/null 2>"$out/sub.err"; then fail "class tiers without the requested class and without a default tier must not render"; fi
grep -q 'openshell.runtimeClassName' "$out/sub.err" || fail "the missing-class error must name openshell.runtimeClassName"
if render "${sub[@]}" --set execution.substrate.defaultTier= >/dev/null 2>"$out/sub.err"; then fail "substrate without a tier must not render"; fi
grep -q 'defaultTier' "$out/sub.err" || fail "the missing-tier error must name execution.substrate.defaultTier"
render "${sub[@]}" --set execution.substrate.defaultTier= "${tiers[@]}" >/dev/null 2>&1 || fail "class tiers alone must render"
if render "${sub[@]}" --set execution.substrate.runtimeClassTiers.kata=root >/dev/null 2>&1; then fail "an unknown tier must not render"; fi

# The gateway NetworkPolicy admits the model port from Substrate's namespace, not OpenShell's, with
# the substrate driver; the default is unchanged.
for gm in standalone kubernetes; do
	gwargs=(--set gateway.mode=$gm)
	has "openshell driver admits OpenShell's namespaces ($gm)" 'openshell.ai/managed-by: openshell' "${gwargs[@]}" --show-only templates/networkpolicy.yaml
	lacks "openshell driver has no Substrate peer ($gm)" 'ate-system' "${gwargs[@]}" --show-only templates/networkpolicy.yaml
	has "substrate admits Substrate's namespace ($gm)" 'kubernetes.io/metadata.name: ate-system' "${sub[@]}" "${gwargs[@]}" --show-only templates/networkpolicy.yaml
	lacks "substrate has no openshell.ai/managed-by peer ($gm)" 'openshell.ai/managed-by' "${sub[@]}" "${gwargs[@]}" --show-only templates/networkpolicy.yaml
done
has "the Substrate peer selector is configurable" 'kubernetes.io/metadata.name: substrate-x' "${sub[@]}" --set execution.substrate.namespace=substrate-x --set execution.substrate.gatewayClientNamespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=substrate-x --show-only templates/networkpolicy.yaml
# Substrate needs every harness image pinned by digest (#1192): the reconciler refuses to start
# without one, so the substrate CI values carry a digest and the config renders it.
# With substrate the default repository is the overlay with the guest server and launcher (#1087).
has "the substrate CI values pin the harness image by digest, and it is the substrate overlay" 'image: .*/sandbox-substrate:[^@ ]+@sha256:[0-9a-f]{64}' -f "$CHART/ci/substrate-values.yaml" --show-only templates/config.yaml
has "a repository set for the harness is used as it is with substrate" 'image: .*/my-sandbox:' "${sub[@]}" --set harnesses.opencode.image.repository=my-sandbox --set harnesses.opencode.image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 --show-only templates/config.yaml
lacks "the default harness image has no digest (OpenShell does not need one)" 'image: .*/sandbox:[^@ ]+@sha256:' --show-only templates/config.yaml
