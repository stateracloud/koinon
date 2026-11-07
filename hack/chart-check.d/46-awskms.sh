# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: wrapping the KEKs with an AWS KMS key (ADR 0085 decision 12, old issue 417).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

awskms_key=arn:aws:kms:eu-central-1:111122223333:key/0b1d2e3f-4a5b-4c6d-8e9f-a0b1c2d3e4f5
awskms=(--set secrets.kek.kms.backend=awskms --set secrets.kek.kms.awskms.region=eu-central-1
	--set "secrets.kek.kms.awskms.keyId=$awskms_key")
awskms_irsa=(--set secrets.kek.kms.awskms.roleArns.api=arn:aws:iam::111122223333:role/koinon-api-decrypt
	--set secrets.kek.kms.awskms.roleArns.reconciler=arn:aws:iam::111122223333:role/koinon-reconciler-decrypt
	--set secrets.kek.kms.awskms.roleArns.keyAdmin=arn:aws:iam::111122223333:role/koinon-key-admin-encrypt)

# The local default renders nothing of AWS.
lacks "no AWS KMS env with the local KEK" 'KOINON_AWSKMS_|eks.amazonaws.com/role-arn'

# The services and the key-admin Job get the key and region; koinon-api no longer creates the KEK
# Secret; the key-admin Job is rendered.
env_is KOINON_KEK_BACKEND awskms "${awskms[@]}"
env_is KOINON_AWSKMS_REGION '"eu-central-1"' "${awskms[@]}"
env_is KOINON_AWSKMS_KEY_ID "\"$awskms_key\"" "${awskms[@]}"
env_is KOINON_KEK_CREATE '"false"' "${awskms[@]}"
has "key-admin Job with AWS KMS" '^            - key-admin$' --show-only templates/key-admin.yaml "${awskms[@]}"
awskms_deploy=$(render --show-only templates/deployments.yaml "${awskms[@]}")
[[ $(grep -c 'name: KOINON_KEK_BACKEND' <<<"$awskms_deploy") == 2 ]] || fail "exactly koinon-api and the reconciler get the AWS KMS settings"
awskms_all=$(render "${awskms[@]}" "${awskms_irsa[@]}")
[[ $(grep -c 'name: KOINON_AWSKMS_KEY_ID' <<<"$awskms_all") == 3 ]] || fail "koinon-api, the reconciler and the key-admin Job get the AWS KMS key"

# No credential: no access key, no OpenBao settings, nothing of AWS's own variables (the IRSA webhook
# or the Pod Identity Agent sets those).
if grep -Eq 'AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN|AWS_ROLE_ARN|AWS_WEB_IDENTITY' <<<"$awskms_all"; then
	fail "the chart sets no AWS credential or identity variable"
fi
if grep -q 'KOINON_OPENBAO_' <<<"$awskms_all"; then fail "no OpenBao settings with backend awskms"; fi

# IRSA: each identity's ServiceAccount carries its own role, decrypt-only for the services.
awskms_sa=$(render --show-only templates/serviceaccounts.yaml "${awskms[@]}" "${awskms_irsa[@]}")
for c in api reconciler; do
	grep -A12 "name: koinon-$c\$" <<<"$awskms_sa" | grep -Fq "eks.amazonaws.com/role-arn: \"arn:aws:iam::111122223333:role/koinon-$c-decrypt\"" ||
		fail "the koinon-$c ServiceAccount carries its IRSA role"
done
if grep -A12 'name: koinon-agentd$' <<<"$awskms_sa" | grep -q 'eks.amazonaws.com'; then fail "agentd gets no IAM role (it holds no keyring)"; fi
has "the key-admin ServiceAccount carries the encrypt role" \
	'eks.amazonaws.com/role-arn: "arn:aws:iam::111122223333:role/koinon-key-admin-encrypt"' \
	--show-only templates/key-admin.yaml "${awskms[@]}" "${awskms_irsa[@]}"
lacks "no IRSA annotation without roleArns (EKS Pod Identity)" 'eks.amazonaws.com/role-arn' "${awskms[@]}"
lacks "no IRSA annotation with another backend" 'eks.amazonaws.com/role-arn' "${awskms_irsa[@]}" \
	--set secrets.kek.kms.backend=openbao --set secrets.kek.kms.openbao.address=https://bao.example:8200 \
	--set secrets.kek.kms.openbao.key=koinon-kek

# Optional settings render only when set; a CA is mounted wherever KMS is called.
lacks "no endpoint, STS endpoint, timeout or CA by default" 'KOINON_AWSKMS_(ENDPOINT|STS_ENDPOINT|TIMEOUT|CA_FILE)' "${awskms[@]}"
env_is KOINON_AWSKMS_ENDPOINT '"https://vpce-0123.kms.eu-central-1.vpce.amazonaws.com"' "${awskms[@]}" \
	--set secrets.kek.kms.awskms.endpoint=https://vpce-0123.kms.eu-central-1.vpce.amazonaws.com
env_is KOINON_AWSKMS_TIMEOUT '"3s"' "${awskms[@]}" --set secrets.kek.kms.awskms.timeout=3s
awskms_ca=$(render "${awskms[@]}" --set secrets.kek.kms.awskms.caConfigMap=proxy-ca)
[[ $(grep -c 'mountPath: /etc/koinon/awskms-ca$' <<<"$awskms_ca") == 3 ]] || fail "AWS KMS CA mounted in koinon-api, the reconciler and the key-admin Job"
[[ $(grep -c 'value: /etc/koinon/awskms-ca/ca.crt$' <<<"$awskms_ca") == 3 ]] || fail "KOINON_AWSKMS_CA_FILE set in koinon-api, the reconciler and the key-admin Job"
grep -Fq 'name: proxy-ca' <<<"$awskms_ca" || fail "the AWS KMS CA volume names the ConfigMap"

# Settings that cannot work fail the render.
for bad in "secrets.kek.kms.awskms.region=" "secrets.kek.kms.awskms.keyId=" \
	"secrets.kek.kms.awskms.keyId=koinon-kek" "secrets.kek.kms.awskms.keyId=alias/aws/secretsmanager" \
	"secrets.kek.kms.awskms.region=us-east-1" "secrets.kek.kms.awskms.endpoint=http://kms.example:4566" \
	"secrets.kek.kms.awskms.stsEndpoint=http://sts.example" "secrets.kek.kms.awskms.roleArns.api=koinon-api"; do
	if render "${awskms[@]}" --set "$bad" >/dev/null 2>&1; then fail "must not render with $bad"; fi
done
has "an alias renders" 'value: "alias/koinon-kek"' "${awskms[@]}" --set secrets.kek.kms.awskms.keyId=alias/koinon-kek

# roleArns: all three (IRSA) or none (EKS Pod Identity); a partial set would leave a pod without a role.
for awskms_partial in api reconciler keyAdmin; do
	if render "${awskms[@]}" --set "secrets.kek.kms.awskms.roleArns.$awskms_partial=arn:aws:iam::111122223333:role/koinon-x" >/dev/null 2>&1; then
		fail "must not render with only roleArns.$awskms_partial"
	fi
done
if render "${awskms[@]}" "${awskms_irsa[@]}" --set secrets.kek.kms.awskms.roleArns.reconciler= >/dev/null 2>&1; then
	fail "must not render with roleArns.reconciler missing"
fi
render "${awskms[@]}" "${awskms_irsa[@]}" >/dev/null 2>&1 || fail "all three roleArns render"
