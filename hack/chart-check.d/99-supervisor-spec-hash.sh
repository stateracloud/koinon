# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the supervisor image in the sandbox spec hash (ADR 0085 decision 9, old issue 410).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# The reconciler is told which OpenShell gateway ConfigMap names the supervisor image, and the cap on
# recreates per pass.
cfg=templates/config.yaml
has "the reconciler is told the gateway ConfigMap namespace" 'namespace: "openshell"' --show-only $cfg
has "the reconciler is told the gateway ConfigMap" 'configMap: "openshell-config"' --show-only $cfg
has "the recreate cap defaults to 4" 'maxRecreatesPerPass: 4' --show-only $cfg
has "the recreate cap is overridable" 'maxRecreatesPerPass: 1' --set reconciler.maxRecreatesPerPass=1 --show-only $cfg
has "preflight.openshell reaches the reconciler" 'configMap: "os-gw-config"' --set preflight.openshell.configMap=os-gw-config --show-only $cfg

# It can get that one ConfigMap and nothing else cluster-wide.
rbac=templates/reconciler-openshell-config.yaml
cr=$(render --show-only $rbac)
grep -Fq -- 'resourceNames: [openshell-config]' <<<"$cr" || fail "the reconciler's OpenShell ClusterRole is pinned to the gateway ConfigMap"
[[ $(grep -c -- 'verbs:' <<<"$cr") == 1 ]] && grep -Fq -- 'verbs: [get]' <<<"$cr" || fail "the reconciler's OpenShell ClusterRole only gets"
has "the ClusterRole is bound to the reconciler" 'name: koinon-reconciler' --show-only $rbac
