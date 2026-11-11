# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: no ServiceAccount token on sandbox workload pods (ADR 0006, old issue 12).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

pol=templates/sandbox-policy.yaml

# Workload pods carry no ServiceAccount token. Supervisor pods set automount false too and may project
# only the openshell-gateway token OpenShell 0.1.7 exchanges at startup (ADR 0006, issue 220).
has "workload pods must set automountServiceAccountToken false" 'object.spec.automountServiceAccountToken == false' --show-only $pol
has "the automount denial message renders" 'sandbox workload pods must set automountServiceAccountToken to false' --show-only $pol
has "projected serviceAccountToken sources are refused" 'has\(s.serviceAccountToken\)' --show-only $pol

# Both token validations are scoped to the workload role: each message must sit under an expression
# that starts with `!variables.workload ||`, so supervisor pods are not refused by them.
pol_out=$(render --show-only $pol)
for msg in \
  "sandbox workload pods must set automountServiceAccountToken to false" \
  "sandbox workload pods must not project a ServiceAccount token"; do
  if ! grep -B4 -F -- "$msg" <<<"$pol_out" | grep -Fq -- '!variables.workload ||'; then
    fail "the workload guard (!variables.workload ||) must scope the validation: $msg"
  fi
done

# Supervisor pods: automount false, only an openshell-gateway audience, a bounded expiry.
has "supervisor pods must set automount false" 'sandbox supervisor pods must set automountServiceAccountToken to false' --show-only $pol
has "supervisor tokens are pinned to the openshell-gateway audience" "audience == 'openshell-gateway'" --show-only $pol
has "supervisor token expiry is required and bounded (default 3600)" 'expirationSeconds <= 3600' --show-only $pol
has "the supervisor expiry message renders" 'must bound the ServiceAccount token to 3600 seconds' --show-only $pol
has "supervisorTokenMaxSeconds overrides the bound" 'expirationSeconds <= 7200' --set sandboxPolicy.supervisorTokenMaxSeconds=7200 --show-only $pol
# The preflight is told the bound, to compare it with the gateway's token lifetime (old issue 419).
has "preflight is told the supervisor token bound" '--supervisor-token-max-seconds=3600' --show-only templates/preflight.yaml
has "preflight is told an overridden supervisor token bound" '--supervisor-token-max-seconds=7200' --set sandboxPolicy.supervisorTokenMaxSeconds=7200 --show-only templates/preflight.yaml
has "preflight reads the gateway workload for the token TTL variable" '--openshell-workload=openshell' --show-only templates/preflight.yaml
lacks "no gateway workload flag when the policy is off" '--openshell-workload' --set sandboxPolicy.enabled=false --set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --show-only templates/preflight.yaml
lacks "no gateway workload get rule when the policy is off" 'resources: [statefulsets, deployments]' --set sandboxPolicy.enabled=false --set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --show-only templates/preflight.yaml
pf_out=$(render --set preflight.openshell.workload=gw-sts --show-only templates/preflight.yaml)
grep -A1 -F -- 'resources: [statefulsets, deployments]' <<<"$pf_out" | grep -Fq -- 'resourceNames: [gw-sts]' ||
	fail "preflight may get only the gateway workload (resourceNames [gw-sts] on the statefulsets/deployments rule)"
has "the preflight gets the gateway workload flag" '--openshell-workload=gw-sts' --set preflight.openshell.workload=gw-sts --show-only templates/preflight.yaml
has "an overridden gateway workload reaches the preflight" '--openshell-workload=gw' --set preflight.openshell.workload=gw --show-only templates/preflight.yaml
lacks "no token bound to check when the policy is off" '--supervisor-token-max-seconds' --set sandboxPolicy.enabled=false --set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --show-only templates/preflight.yaml
for msg in \
  "sandbox supervisor pods must set automountServiceAccountToken to false" \
  "sandbox supervisor pods may project only a ServiceAccount token for the audience openshell-gateway" \
  "sandbox supervisor pods must bound the ServiceAccount token to 3600 seconds"; do
  if ! grep -B4 -F -- "$msg" <<<"$pol_out" | grep -Fq -- '!variables.supervisor ||'; then
    fail "the supervisor guard (!variables.supervisor ||) must scope the validation: $msg"
  fi
done

# Pods from trusted identities must carry boundary-role workload or supervisor (issue 418), so one of
# the two rule sets always applies.
has "pods must carry a boundary role" 'variables.workload \|\| variables.supervisor' --show-only $pol
has "the boundary role message renders" 'sandbox pods must carry the label openshell.ai/boundary-role=workload or openshell.ai/boundary-role=supervisor' --show-only $pol
