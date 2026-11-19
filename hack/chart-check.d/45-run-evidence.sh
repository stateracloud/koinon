# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the run-evidence export Job (#966, ADR 0121).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

re=(--show-only templates/run-evidence-export.yaml)
reon=(--set runEvidence.export.enabled=true
	--set runEvidence.export.org=00000000-0000-4000-8000-000000000001
	--set runEvidence.export.run=00000000-0000-4000-8000-000000000002
	--set runEvidence.export.out=s3://evidence-bucket/run-2
	--set runEvidence.export.s3.existingSecret=koinon-evidence-s3)

# Off by default: no Job.
lacks "no run-evidence Job by default" 'run-evidence'

has "run-evidence Job exports the run" '^            - export$' "${re[@]}" "${reon[@]}"
has "run-evidence Job names the org" '"--org=00000000-0000-4000-8000-000000000001"' "${re[@]}" "${reon[@]}"
has "run-evidence Job names the run" '"--run=00000000-0000-4000-8000-000000000002"' "${re[@]}" "${reon[@]}"
has "run-evidence Job reads the KEKs through the API, not a mount" '"--kek-secret=koinon-kek"' "${re[@]}" "${reon[@]}"
lacks "run-evidence Job has no --no-org-secrets" 'no-org-secrets' "${re[@]}" "${reon[@]}"
has "run-evidence Job takes the S3 keys from the Secret" 'name: "koinon-evidence-s3"' "${re[@]}" "${reon[@]}"
has "run-evidence Job uses the api service account (no new RBAC)" '^      serviceAccountName: koinon-api$' "${re[@]}" "${reon[@]}"
has "run-evidence Job reads the database" 'name: DATABASE_URL' "${re[@]}" "${reon[@]}"
re_out=$(render "${re[@]}" "${reon[@]}")
if grep -Eq 'kind: (Role|RoleBinding|ServiceAccount)' <<<"$re_out"; then fail "run-evidence Job adds no RBAC or service account"; fi
if grep -Eq 'AWS_SECRET_ACCESS_KEY.*value:|secret.*value: ' <<<"$re_out"; then fail "run-evidence Job holds no key in values"; fi
if grep -q 'helm.sh/hook' <<<"$re_out"; then fail "run-evidence Job is not a Helm hook"; fi
