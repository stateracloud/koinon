# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the read-only second wrapping key of a key move (issue 688, issue 1298).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, hit, ...).

ro_new=arn:aws:kms:eu-central-1:111122223333:key/0b1d2e3f-4a5b-4c6d-8e9f-a0b1c2d3e4f5
ro_old=arn:aws:kms:eu-central-1:111122223333:key/1c2d3e4f-5a6b-4c7d-8e9f-b0c1d2e3f4a5
ro_primary=(--set secrets.kek.kms.backend=awskms --set secrets.kek.kms.awskms.region=eu-central-1
	--set "secrets.kek.kms.awskms.keyId=$ro_new")
ro_aws=(--set secrets.kek.kms.readOnly.backend=awskms --set secrets.kek.kms.readOnly.awskms.region=eu-central-1
	--set "secrets.kek.kms.readOnly.awskms.keyId=$ro_old")
ro_bao=(--set secrets.kek.kms.readOnly.backend=openbao --set secrets.kek.kms.readOnly.openbao.address=https://old-bao.example:8200
	--set secrets.kek.kms.readOnly.openbao.key=koinon-kek-old)

# Nothing by default, with the local KEK or with a KMS primary alone.
lacks "no read-only key env by default" 'KOINON_READ_ONLY_|ro-awskms-ca|ro-openbao-ca'
lacks "no read-only key env with a KMS primary alone" 'KOINON_READ_ONLY_|ro-awskms-ca|ro-openbao-ca' "${ro_primary[@]}"

# AWS KMS: the flags reach koinon-api and the reconciler (deployments), the key-admin Job and the
# run-evidence Job; the key ID is the old one; no credential and no AWS_* variable.
env_is KOINON_READ_ONLY_KEK_BACKEND awskms "${ro_primary[@]}" "${ro_aws[@]}"
env_is KOINON_READ_ONLY_AWSKMS_REGION '"eu-central-1"' "${ro_primary[@]}" "${ro_aws[@]}"
env_is KOINON_READ_ONLY_AWSKMS_KEY_ID "\"$ro_old\"" "${ro_primary[@]}" "${ro_aws[@]}"
env_is KOINON_AWSKMS_KEY_ID "\"$ro_new\"" "${ro_primary[@]}" "${ro_aws[@]}"
ro_deploy=$(render --show-only templates/deployments.yaml "${ro_primary[@]}" "${ro_aws[@]}")
[[ $(grep -c 'name: KOINON_READ_ONLY_KEK_BACKEND' <<<"$ro_deploy") == 2 ]] || fail "exactly koinon-api and the reconciler get the read-only key"
ro_all=$(render "${ro_primary[@]}" "${ro_aws[@]}")
[[ $(grep -c 'name: KOINON_READ_ONLY_AWSKMS_KEY_ID' <<<"$ro_all") == 3 ]] || fail "koinon-api, the reconciler and the key-admin Job get the read-only key"
if hit -E 'KOINON_READ_ONLY_OPENBAO_|AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN' <<<"$ro_all"; then
	fail "the read-only AWS KMS key sets no OpenBao setting, credential or AWS_* variable"
fi
ro_agentd=$(render --show-only templates/deployments.yaml "${ro_primary[@]}" "${ro_aws[@]}" | awk '/name: koinon-agentd$/{f=1} f')
if hit 'KOINON_READ_ONLY_' <<<"$ro_agentd"; then fail "agentd holds no keyring and gets no read-only key"; fi

# Optional settings render only when set; the CA is mounted where the flags are.
lacks "no read-only endpoint, STS endpoint, timeout or CA by default" 'KOINON_READ_ONLY_AWSKMS_(ENDPOINT|STS_ENDPOINT|TIMEOUT|CA_FILE)' "${ro_primary[@]}" "${ro_aws[@]}"
env_is KOINON_READ_ONLY_AWSKMS_ENDPOINT '"https://vpce-0456.kms.eu-central-1.vpce.amazonaws.com"' "${ro_primary[@]}" "${ro_aws[@]}" \
	--set secrets.kek.kms.readOnly.awskms.endpoint=https://vpce-0456.kms.eu-central-1.vpce.amazonaws.com
env_is KOINON_READ_ONLY_AWSKMS_TIMEOUT '"3s"' "${ro_primary[@]}" "${ro_aws[@]}" --set secrets.kek.kms.readOnly.awskms.timeout=3s
ro_ca=$(render "${ro_primary[@]}" "${ro_aws[@]}" --set secrets.kek.kms.readOnly.awskms.caConfigMap=old-ca)
[[ $(grep -c 'mountPath: /etc/koinon/ro-awskms-ca$' <<<"$ro_ca") == 3 ]] || fail "the read-only CA is mounted in koinon-api, the reconciler and the key-admin Job"
[[ $(grep -c 'value: /etc/koinon/ro-awskms-ca/ca.crt$' <<<"$ro_ca") == 3 ]] || fail "KOINON_READ_ONLY_AWSKMS_CA_FILE is set in koinon-api, the reconciler and the key-admin Job"
hit -F 'name: old-ca' <<<"$ro_ca" || fail "the read-only CA volume names the ConfigMap"
lacks "the primary's CA path is not the read-only one" '/etc/koinon/awskms-ca' "${ro_primary[@]}" "${ro_aws[@]}" --set secrets.kek.kms.readOnly.awskms.caConfigMap=old-ca

# OpenBao: each identity signs in as its own role; the old key is another transit key.
bao_primary=(--set secrets.kek.kms.backend=openbao --set secrets.kek.kms.openbao.address=https://bao.example:8200
	--set secrets.kek.kms.openbao.key=koinon-kek)
env_is KOINON_READ_ONLY_KEK_BACKEND openbao "${bao_primary[@]}" "${ro_bao[@]}"
env_is KOINON_READ_ONLY_OPENBAO_ADDRESS '"https://old-bao.example:8200"' "${bao_primary[@]}" "${ro_bao[@]}"
env_is KOINON_READ_ONLY_OPENBAO_KEY '"koinon-kek-old"' "${bao_primary[@]}" "${ro_bao[@]}"
env_is KOINON_READ_ONLY_OPENBAO_ROLE '"koinon-api"' "${bao_primary[@]}" "${ro_bao[@]}"
env_is KOINON_OPENBAO_KEY '"koinon-kek"' "${bao_primary[@]}" "${ro_bao[@]}"
bao_all=$(render "${bao_primary[@]}" "${ro_bao[@]}")
hit -F 'value: "koinon-key-admin"' <<<"$bao_all" || fail "the key-admin Job signs in to the old key as the key-admin role"
[[ $(grep -c 'name: KOINON_READ_ONLY_OPENBAO_KEY' <<<"$bao_all") == 3 ]] || fail "koinon-api, the reconciler and the key-admin Job get the read-only OpenBao key"
# A move back to local: the primary is local, the old KMS key is read-only.
env_is KOINON_READ_ONLY_KEK_BACKEND openbao "${ro_bao[@]}"
lacks "a local primary has no KMS primary env" 'KOINON_KEK_BACKEND' "${ro_bao[@]}"
ro_ca_bao=$(render "${bao_primary[@]}" "${ro_bao[@]}" --set secrets.kek.kms.readOnly.openbao.caConfigMap=old-bao-ca --set secrets.kek.kms.openbao.caConfigMap=bao-ca)
[[ $(grep -c 'mountPath: /etc/koinon/ro-openbao-ca$' <<<"$ro_ca_bao") == 3 ]] || fail "the read-only OpenBao CA is mounted next to the primary's"
ro_ca_bao_plain=$(render "${bao_primary[@]}" --set secrets.kek.kms.openbao.caConfigMap=bao-ca)
[[ $(grep -c 'mountPath: /etc/koinon/openbao-ca$' <<<"$ro_ca_bao") == $(grep -c 'mountPath: /etc/koinon/openbao-ca$' <<<"$ro_ca_bao_plain") ]] ||
	fail "the read-only key changes none of the primary's CA mounts"

# A key move across backends: OpenBao primary, AWS read-only key (and the reverse) render.
env_is KOINON_READ_ONLY_AWSKMS_KEY_ID "\"$ro_old\"" "${bao_primary[@]}" "${ro_aws[@]}"
env_is KOINON_READ_ONLY_OPENBAO_KEY '"koinon-kek-old"' "${ro_primary[@]}" "${ro_bao[@]}"

# Refusals: the same key twice, a missing setting, an unknown backend.
if render "${ro_primary[@]}" --set secrets.kek.kms.readOnly.backend=awskms --set secrets.kek.kms.readOnly.awskms.region=eu-central-1 \
	--set "secrets.kek.kms.readOnly.awskms.keyId=$ro_new" >/dev/null 2>"$out/ro-same-aws.err"; then
	fail "the read-only AWS key equal to the primary must not render"
fi
if render "${bao_primary[@]}" --set secrets.kek.kms.readOnly.backend=openbao --set secrets.kek.kms.readOnly.openbao.address=https://old-bao.example:8200 \
	--set secrets.kek.kms.readOnly.openbao.key=koinon-kek >/dev/null 2>"$out/ro-same-bao.err"; then
	fail "the read-only OpenBao key equal to the primary must not render"
fi
if render --set secrets.kek.kms.readOnly.backend=awskms >/dev/null 2>"$out/ro-aws-missing.err"; then
	fail "a read-only AWS backend without region and key must not render"
fi
if render --set secrets.kek.kms.readOnly.backend=openbao >/dev/null 2>"$out/ro-bao-missing.err"; then
	fail "a read-only OpenBao backend without address and key must not render"
fi
if render --set secrets.kek.kms.readOnly.backend=local >/dev/null 2>"$out/ro-local.err"; then
	fail "a local read-only backend must not render (a local KEK always opens)"
fi
