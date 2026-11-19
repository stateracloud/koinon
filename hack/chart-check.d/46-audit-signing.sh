# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the audit checkpoint signing Secret (issue #334, ADR 0100 decision 4.2).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).
#
# koinon-api makes the Secret koinon-audit-signing on its first checkpoint and is the only workload
# that reads it, through the Kubernetes API. So the chart must (a) pass koinon-api the name, (b) never
# render the Secret (an upgrade would overwrite the key), (c) never mount it or reference it from any
# other workload, and (d) give no Role a rule that names it.

sign=koinon-audit-signing
kms=(--set secrets.kek.kms.backend=openbao --set secrets.kek.kms.openbao.address=https://bao.example:8200 --set secrets.kek.kms.openbao.key=koinon-kek)

has "koinon-api gets the audit signing Secret name" "\"--audit-signing-secret\", \"$sign\"" --show-only templates/deployments.yaml

# audit_signing_refs <helm args...>: the lines of the rendering that carry the Secret name.
audit_signing_refs() { render "$@" | grep -F -- "$sign" || true; }
for variant in "default" "kms" "durable"; do
	case $variant in
	default) vargs=() ;;
	kms) vargs=("${kms[@]}") ;;
	durable) vargs=(--set api.durable.enabled=true) ;;
	esac
	refs=$(audit_signing_refs "${vargs[@]}")
	n=$(grep -o -F -- "$sign" <<<"$refs" | wc -l | tr -d ' ')
	[ "$n" = 1 ] || fail "the audit signing Secret name appears once in the $variant rendering (the koinon-api args), not $n times"
	grep -Fq -- '"--saved-work-secret"' <<<"$refs" || fail "the one reference to the audit signing Secret is koinon-api's serve args ($variant)"
done

# Helm never renders the Secret, a volume of it, a secretKeyRef to it, or a Role rule that names it.
lacks "the chart does not render the audit signing Secret" "name: $sign\$"
lacks "no workload mounts the audit signing Secret" "secretName: $sign"
lacks "no workload reads the audit signing Secret into its env" "secretKeyRef:[[:space:]]*\$[^-]*name: $sign"
lacks "no Role names the audit signing Secret" "resourceNames:.*$sign"

# koinon-api's Role can make and read Secrets but update only the KEK Secret by name: the signing Secret is
# never overwritten. (The Role has no rule for the signing Secret of its own.)
api_role=$(render --show-only templates/rbac.yaml | awk 'BEGIN{RS="\n---\n"} /kind: Role\n/ && /name: koinon-api\n/ && !/RoleBinding/')
grep -Eq 'verbs: \[get, list, watch, create\]' <<<"$api_role" || fail "koinon-api's Role gets and creates Secrets (it makes the audit signing Secret)"
other_writes=$(grep -E 'verbs: \[[^]]*(update|patch)' <<<"$api_role" | grep -Ev 'verbs: \[update\]$' || true)
[ -z "$other_writes" ] || fail "koinon-api's Role updates or patches only through the KEK rule that names the KEK Secret"
upd=$(grep -B2 'verbs: \[update\]' <<<"$api_role" || true)
grep -Fq 'resourceNames: [koinon-kek]' <<<"$upd" || fail "koinon-api's only Secret update rule names the KEK Secret"

# The Secret koinon-api makes carries the managed-by label of koinon-api, not of the reconciler, so the
# reconciler fence (kek-guard.yaml) refuses its writes to it.
has "the reconciler Secret fence names the reconciler label" 'koinon.dev/managed-by' --show-only templates/kek-guard.yaml
lacks "no Role other than koinon-api's has a Secret rule that names the audit signing Secret" "$sign" --show-only templates/rbac.yaml --show-only templates/key-admin.yaml "${kms[@]}"
