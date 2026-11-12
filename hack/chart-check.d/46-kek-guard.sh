# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the KEK Secret guard and the reconciler Secret fence (issue #604, ADR 0056
# amendment 2026-11-11). Sourced in order by hack/chart-check.sh; it uses that script's helpers.

kg=templates/kek-guard.yaml

# On by default: two native ValidatingAdmissionPolicies (never Kyverno) with Deny bindings, bound to
# the release namespace.
has "KEK guard policy" 'name: koinon-koinon-kek-guard$' --show-only "$kg"
has "KEK guard names the KEK Secret" 'resourceNames: \["koinon-kek"\]' --show-only "$kg"
has "KEK guard covers UPDATE and DELETE" 'operations: \[UPDATE, DELETE\]' --show-only "$kg"
has "KEK guard fails closed" 'failurePolicy: Fail' --show-only "$kg"
has "KEK guard binding denies" 'validationActions: \["Deny"\]' --show-only "$kg"
has "KEK guard bound to the release namespace" 'kubernetes.io/metadata.name: "koinon"' --show-only "$kg"
has "KEK guard refuses DELETE" "request.operation != 'DELETE' \|\| variables.terminating" --show-only "$kg"
has "KEK guard lets koinon-api write" 'system:serviceaccount:koinon:koinon-api' --show-only "$kg"
has "KEK guard lets the key-admin Job write" 'system:serviceaccount:koinon:koinon-key-admin' --show-only "$kg"
lacks "KEK guard does not let the reconciler write the KEK" 'writers.*koinon-reconciler' --show-only "$kg"
has "KEK guard follows existingSecret" 'resourceNames: \["mine"\]' --set secrets.kek.existingSecret=mine --show-only "$kg"
has "KEK guard extraWriters" 'argocd-application-controller' --set 'secrets.kek.guard.extraWriters={system:serviceaccount:argocd:argocd-application-controller}' --show-only "$kg"
lacks "KEK guard has no Kyverno" 'kyverno'

# The reconciler may write only Secrets it owns (label koinon.dev/managed-by=koinon-reconciler).
has "reconciler Secret fence policy" 'name: koinon-koinon-reconciler-secrets$' --show-only "$kg"
has "reconciler Secret fence keys on the reconciler identity" 'request.userInfo.username == \\"system:serviceaccount:koinon:koinon-reconciler\\"' --show-only "$kg"
has "reconciler Secret fence covers CREATE UPDATE DELETE" 'operations: \[CREATE, UPDATE, DELETE\]' --show-only "$kg"
has "reconciler Secret fence needs the managed-by label" "koinon-reconciler'" --show-only "$kg"
# The chart-rendered provider keys Secret carries the label, or the reconciler could not fill it.
has "provider keys Secret (standalone) is labelled for the fence" 'koinon.dev/managed-by: koinon-reconciler' --show-only templates/gateway-standalone.yaml
has "provider keys Secret (kubernetes) is labelled for the fence" 'koinon.dev/managed-by: koinon-reconciler' --set gateway.mode=kubernetes --show-only templates/gateway-kubernetes.yaml

lacks "secrets.kek.guard.enabled=false renders no KEK guard" 'name: koinon-koinon-kek-guard' --set secrets.kek.guard.enabled=false
# The fence has its own switch: it stays rendered with the KEK guard off, and goes with its own.
has "reconciler Secret fence renders with the KEK guard off" 'name: koinon-koinon-reconciler-secrets$' --set secrets.kek.guard.enabled=false --show-only "$kg"
lacks "reconciler Secret fence is not rendered with secrets.reconcilerFence.enabled=false" 'reconciler-secrets' --set secrets.reconcilerFence.enabled=false
has "KEK guard renders with the fence off" 'name: koinon-koinon-kek-guard$' --set secrets.reconcilerFence.enabled=false --show-only "$kg"
