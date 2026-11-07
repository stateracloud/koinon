# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: no ServiceAccount token on sandbox workload pods (ADR 0006, old issue 12).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

pol=templates/sandbox-policy.yaml

# Workload pods carry no ServiceAccount token. Supervisor pods are not covered: the gateway creates
# them and the supervisor's needs are unverified.
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
