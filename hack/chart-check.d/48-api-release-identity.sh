# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: koinon-api's Helm release identity scopes the generated-Secret selector (#375).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# The chart is the only place that knows the release name; koinon-api needs it to list only this
# release's ensure-secrets Secrets (koinon.dev/release) when two releases share a namespace.
has "koinon-api is told the Helm release name" '"--release", "koinon"' --show-only templates/deployments.yaml
# A different release name reaches the flag (render always uses release "koinon"; call helm directly).
rel_out=$("$HELM" template other "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" --show-only templates/deployments.yaml)
grep -Fq -- '"--release", "other"' <<<"$rel_out" || fail "koinon-api gets --release from .Release.Name (got other)"
# ensure-secrets already labels generated Secrets with the same release identity (JSON in --secrets=
# with escaped quotes inside the YAML string).
has "generated Secrets carry koinon.dev/release" 'koinon\.dev/release\\":\\"koinon' --show-only templates/ensure-secrets.yaml --set postgres.mode=cnpg
