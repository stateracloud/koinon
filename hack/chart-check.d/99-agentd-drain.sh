# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: koinon-agentd drains on SIGTERM (ADR 0085, old issue 403).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# The drain deadline reaches agentd, and the pod's grace period outlasts it by 30 s: otherwise the
# kubelet kills the pod mid-drain and the run leases are never released.
env_is KOINON_DRAIN_DEADLINE '"300s"'
env_is KOINON_DRAIN_DEADLINE '"90s"' --set agentd.drainDeadlineSeconds=90
agentd_grace() { render --show-only templates/deployments.yaml "$@" | awk 'BEGIN{RS="\n---\n"} /component: agentd/ && match($0, /terminationGracePeriodSeconds: [0-9]+/){print substr($0, RSTART+31, RLENGTH-31)}'; }
[[ $(agentd_grace) == 330 ]] || fail "agentd terminationGracePeriodSeconds is the drain deadline plus 30 (330), got '$(agentd_grace)'"
[[ $(agentd_grace --set agentd.drainDeadlineSeconds=90) == 120 ]] || fail "agentd terminationGracePeriodSeconds follows agentd.drainDeadlineSeconds"
