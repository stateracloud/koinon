# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: wrapping the KEKs with a KMS and the key-admin Job (ADR 0085 decision 12, old issue 416).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

kms=(--set secrets.kek.kms.backend=openbao --set secrets.kek.kms.openbao.address=https://bao.example:8200
	--set secrets.kek.kms.openbao.key=koinon-kek)
ka=(--show-only templates/key-admin.yaml)

# Local is the default: no Job, no KMS settings, koinon-api creates the KEK Secret as before.
lacks "no key-admin objects with the local KEK" 'koinon-key-admin'
lacks "no KMS env with the local KEK" 'KOINON_KEK_BACKEND|KOINON_OPENBAO_'
env_is KOINON_KEK_CREATE '"true"'

# With OpenBao: one Job with its own service account, the only identity that wraps.
has "key-admin ServiceAccount" '^  name: koinon-key-admin$' "${ka[@]}" "${kms[@]}"
has "key-admin Job runs key-admin" '^            - key-admin$' "${ka[@]}" "${kms[@]}"
has "key-admin Job uses its own ServiceAccount" '^      serviceAccountName: koinon-key-admin$' "${ka[@]}" "${kms[@]}"
has "key-admin Job signs in as its role" 'value: "koinon-key-admin"' "${ka[@]}" "${kms[@]}"
has "key-admin Job creates the KEK Secret" '^            - --kek-create$' "${ka[@]}" "${kms[@]}"
has "key-admin Job reads the database (the install ID, the re-create guard)" 'name: DATABASE_URL' "${ka[@]}" "${kms[@]}"
lacks "key-admin Job adds no KEK unless asked" 'add-key' "${ka[@]}" "${kms[@]}"
# Binding the KEKs wrapped before old issue 615 is opt-in: a binary older than old issue 615 cannot read a bound KEK.
lacks "key-admin Job binds no old KEK unless asked (old issue 615)" 'rewrap-unbound' "${ka[@]}" "${kms[@]}"
has "key-admin Job binds the old KEKs when asked (old issue 615)" '^            - --rewrap-unbound$' "${ka[@]}" "${kms[@]}" \
	--set secrets.kek.kms.keyAdmin.rewrapUnbound=true
has "key-admin Job adds the asked-for KEK" '"--add-key=kek-20261103-aaaa"' "${ka[@]}" "${kms[@]}" \
	--set secrets.kek.kms.keyAdmin.addKey=kek-20261103-aaaa
# Recovery from a lost KMS key that wrapped the active KEK (issue #255): opt-in, with an ID to add.
lacks "key-admin Job activates nothing unless asked (issue #255)" 'activate-if-no-active' "${ka[@]}" "${kms[@]}" \
	--set secrets.kek.kms.keyAdmin.addKey=kek-20261103-aaaa
has "key-admin Job makes the added KEK active when there is none (issue #255)" '^            - --activate-if-no-active$' "${ka[@]}" "${kms[@]}" \
	--set secrets.kek.kms.keyAdmin.addKey=kek-20261103-aaaa --set secrets.kek.kms.keyAdmin.activateIfNoActive=true
ka_out=$(render "${ka[@]}" "${kms[@]}")
grep -Fq -- 'resourceNames: [koinon-kek]' <<<"$ka_out" || fail "key-admin Role is limited to the KEK Secret by name"
grep -Fq -- 'verbs: [get, update]' <<<"$ka_out" || fail "key-admin Role reads and updates the KEK Secret"
if grep -Eq 'verbs: \[.*(delete|list|watch|patch)' <<<"$ka_out"; then fail "key-admin Role deletes, lists or watches nothing"; fi
if grep -q 'argocd.argoproj.io/hook' <<<"$ka_out"; then fail "key-admin Job is not an Argo CD hook (it would rerun on every sync)"; fi
if grep -q 'helm.sh/hook' <<<"$ka_out"; then fail "key-admin Job is not a Helm hook (post-install deadlocks --wait; cnpg has no database pre-install)"; fi

# The Job is named by its pod template: same inputs, same name (an unchanged upgrade or sync leaves
# the finished Job alone); new inputs, new name (it runs once more, and no immutable spec changes).
job_name() { render "${ka[@]}" "${kms[@]}" "$@" | grep -A2 '^kind: Job$' | sed -n 's/^  name: //p'; }
n1=$(job_name) n2=$(job_name) n3=$(job_name --set secrets.kek.kms.keyAdmin.addKey=kek-20261103-aaaa)
[[ $n1 =~ ^koinon-key-admin-[0-9a-f]{10}$ ]] || fail "key-admin Job name is koinon-key-admin-<hash>, got '$n1'"
[[ $n1 == "$n2" ]] || fail "key-admin Job name is stable across renders ($n1, $n2)"
[[ $n1 != "$n3" ]] || fail "key-admin Job name changes with addKey"
n4=$(job_name --set image.tag=9.9.9)
[[ $n1 != "$n4" ]] || fail "key-admin Job name changes with the image"
n5=$(job_name --set secrets.kek.kms.keyAdmin.rewrapUnbound=true)
[[ $n1 != "$n5" ]] || fail "key-admin Job name changes with rewrapUnbound"
# A long release name cuts the prefix, never the hash: otherwise a changed template would keep the
# name and the upgrade would fail on the Job's immutable spec.
long=(--set fullnameOverride=koinon-a-release-name-long-enough-to-fill-sixty-three-chars)
l1=$(job_name "${long[@]}") l2=$(job_name "${long[@]}" --set secrets.kek.kms.keyAdmin.addKey=kek-20261103-aaaa)
[[ ${#l1} -le 63 && $l1 =~ -[0-9a-f]{10}$ ]] || fail "key-admin Job name keeps its hash within 63 characters, got '$l1'"
[[ $l1 != "$l2" ]] || fail "key-admin Job name changes with addKey under a long fullname ($l1)"

# The Job accepts no connections, as the migrate Job.
np=$(render --show-only templates/networkpolicy.yaml "${kms[@]}")
grep -A20 '^  name: koinon-key-admin$' <<<"$np" | grep -q 'policyTypes: \[Ingress\]' || fail "key-admin NetworkPolicy denies ingress"
lacks "no key-admin NetworkPolicy with the local KEK" 'name: koinon-key-admin$' --show-only templates/networkpolicy.yaml

# The services decrypt only: each signs in as its own role, and koinon-api no longer creates the
# KEK Secret (only the Job can wrap its first KEK).
env_is KOINON_KEK_BACKEND openbao "${kms[@]}"
env_is KOINON_KEK_CREATE '"false"' "${kms[@]}"
deploy=$(render --show-only templates/deployments.yaml "${kms[@]}")
for c in api reconciler; do
	grep -Fq -- "value: \"koinon-$c\"" <<<"$deploy" || fail "koinon-$c signs in to OpenBao as role koinon-$c"
done
if grep -Fq 'value: "koinon-key-admin"' <<<"$deploy"; then fail "no Deployment signs in as the key-admin role"; fi
[[ $(grep -c 'name: KOINON_KEK_BACKEND' <<<"$deploy") == 2 ]] || fail "exactly koinon-api and the reconciler get the KMS settings (agentd holds no keyring)"
env_is KOINON_OPENBAO_ROLE '"bao-api"' "${kms[@]}" --set secrets.kek.kms.openbao.roles.api=bao-api

# The operator's own KEK Secret is never created: no --kek-create, no create right. The Job still
# reads the database: every KEK it adds is bound to the install ID there (old issue 615).
ka_out=$(render "${ka[@]}" "${kms[@]}" --set secrets.kek.existingSecret=my-kek)
if grep -q -- '--kek-create' <<<"$ka_out"; then fail "no --kek-create with secrets.kek.existingSecret"; fi
if grep -Fq -- 'verbs: [create]' <<<"$ka_out"; then fail "no create right with secrets.kek.existingSecret"; fi
grep -q 'name: DATABASE_URL' <<<"$ka_out" || fail "the Job reads the install ID with secrets.kek.existingSecret too (old issue 615)"
grep -Fq -- '"--kek-secret=my-kek"' <<<"$ka_out" || fail "the Job adds to secrets.kek.existingSecret"

# The OpenBao CA, when named, is mounted wherever the KMS is called.
ka_out=$(render "${kms[@]}" --set secrets.kek.kms.openbao.caConfigMap=bao-ca)
[[ $(grep -c 'mountPath: /etc/koinon/openbao-ca$' <<<"$ka_out") == 3 ]] || fail "OpenBao CA mounted in koinon-api, the reconciler and the key-admin Job"
[[ $(grep -c 'value: /etc/koinon/openbao-ca/ca.crt$' <<<"$ka_out") == 3 ]] || fail "KOINON_OPENBAO_CA_FILE set in koinon-api, the reconciler and the key-admin Job"
lacks "no OpenBao CA mount without caConfigMap" 'openbao-ca' "${kms[@]}"

# Settings that cannot work fail the render.
for bad in "secrets.kek.kms.openbao.address=" "secrets.kek.kms.openbao.key=" \
	"secrets.kek.kms.openbao.address=http://bao.example:8200" "secrets.kek.kms.backend=vault"; do
	if render "${kms[@]}" --set "$bad" >/dev/null 2>&1; then fail "must not render with $bad"; fi
done
if render --set secrets.kek.kms.keyAdmin.addKey=kek-20261103-aaaa >/dev/null 2>&1; then
	fail "keyAdmin.addKey without a KMS must not render"
fi
for bad_id in Not_An_ID active; do
	if render "${kms[@]}" --set secrets.kek.kms.keyAdmin.addKey=$bad_id >/dev/null 2>&1; then
		fail "keyAdmin.addKey must be a KEK ID, not $bad_id"
	fi
done
has "plain http on the loopback renders" 'KOINON_OPENBAO_ADDRESS' "${kms[@]}" --set secrets.kek.kms.openbao.address=http://127.0.0.1:8200
