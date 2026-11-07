# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: agentd rollout strategy and PodDisruptionBudget (ADR 0085, old issue 405).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

doc=$(render --show-only templates/deployments.yaml | awk 'BEGIN{RS="\n---\n"} /component: agentd/')
for want in 'type: RollingUpdate' 'maxSurge: 1' 'maxUnavailable: 0'; do
  grep -q "$want" <<<"$doc" || fail "agentd Deployment sets $want"
done
# The API and the reconciler keep Kubernetes' default strategy.
for c in api reconciler; do
  render --show-only templates/deployments.yaml | awk -v c="component: $c" 'BEGIN{RS="\n---\n"} index($0, c) && /maxSurge/{bad=1} END{exit bad}' \
    || fail "$c Deployment must not set the agentd rollout strategy"
done

# One replica: no budget (it would block every node drain). Two or more: one pod unavailable at a time.
pdb() { render "$@" | awk 'BEGIN{RS="\n---\n"} /kind: PodDisruptionBudget/'; }
[[ -z $(pdb) ]] || fail "no PodDisruptionBudget with agentd.replicas=1"
pdb2=$(pdb --set agentd.replicas=2)
grep -q 'maxUnavailable: 1' <<<"$pdb2" || fail "agentd PodDisruptionBudget with replicas=2 sets maxUnavailable 1"
grep -q 'component: agentd' <<<"$pdb2" || fail "agentd PodDisruptionBudget selects the agentd pods"
grep -q 'maxUnavailable: 2' <<<"$(pdb --set agentd.replicas=4 --set agentd.podDisruptionBudget.maxUnavailable=2)" \
  || fail "agentd PodDisruptionBudget follows agentd.podDisruptionBudget.maxUnavailable"
[[ $(pdb --set agentd.replicas=3 --set api.replicas=3 --set reconciler.replicas=3 | grep -c 'kind: PodDisruptionBudget') == 1 ]] \
  || fail "only agentd gets a PodDisruptionBudget"
