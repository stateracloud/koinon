# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: home checkpoints.
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# Home checkpoints (plan item HC1): off by default; on, koinon-agentd (the only holder of the S3
# credentials) gets the bucket, the Secret's keys and a scratch volume (no KEK), and the reconciler
# is told. Nothing renders for the S3 credentials in any other component.
ckpt=(--set checkpoints.enabled=true --set checkpoints.s3.existingSecret=koinon-homes-s3 --set checkpoints.s3.bucket=koinon-homes)
lacks "no checkpoint flags by default" 'KOINON_CHECKPOINTS|AWS_ACCESS_KEY_ID'
lacks "the reconciler is not told to checkpoint by default" 'checkpoints: true'
env_is KOINON_CHECKPOINTS '"true"' "${ckpt[@]}"
env_is KOINON_CHECKPOINTS_S3_BUCKET '"koinon-homes"' "${ckpt[@]}"
env_is KOINON_CHECKPOINTS_INTERVAL '"1h"' "${ckpt[@]}"
has "the reconciler is told to checkpoint" 'checkpoints: true' "${ckpt[@]}" --show-only templates/config.yaml
has "the S3 credentials come from the Secret" 'name: "koinon-homes-s3"' "${ckpt[@]}"
secret_key() { render --show-only templates/deployments.yaml "${ckpt[@]}" | grep -A4 -E -- "- name: $1\$" | hit -F -- "key: $2" || fail "$1 reads key $2 of the Secret"; }
secret_key AWS_ACCESS_KEY_ID AWS_ACCESS_KEY_ID
secret_key AWS_SECRET_ACCESS_KEY AWS_SECRET_ACCESS_KEY
secret_key KOINON_CHECKPOINTS_S3_ENDPOINT endpoint
secret_key KOINON_CHECKPOINTS_S3_REGION region
has "agentd gets a scratch volume with the size limit" 'sizeLimit: "9Gi"' "${ckpt[@]}" --set checkpoints.scratchSize=9Gi
# The default holds 2 backups and 1 restore of 4Gi each plus caches (values.yaml).
has "the default scratch size covers the backups, a restore and the caches" 'sizeLimit: "14Gi"' "${ckpt[@]}"
agentd_kekenv=$(render --show-only templates/deployments.yaml "${ckpt[@]}" | awk 'BEGIN{RS="\n---\n"} /name: koinon-agentd|component: agentd/ && /KOINON_KEK_DIR/{n++} END{print n+0}')
[[ $agentd_kekenv == 0 ]] || fail "koinon-agentd must not get KOINON_KEK_DIR (koinon-api hands out the repository passwords, ADR 0054)"
for mode in default checkpoints; do
  args=(); [[ $mode == checkpoints ]] && args=("${ckpt[@]}")
  rec_kek=$(render --show-only templates/deployments.yaml ${args[@]+"${args[@]}"} | awk 'BEGIN{RS="\n---\n"} /name: koinon-reconciler/ && /name: KOINON_KEK_DIR/ && /\/secrets\/kek/{n++} END{print n+0}')
  [[ $rec_kek == 1 ]] || fail "the reconciler must render KOINON_KEK_DIR=/secrets/kek ($mode mode), found $rec_kek"
done
agentd_kek=$(render --show-only templates/deployments.yaml "${ckpt[@]}" | awk 'BEGIN{RS="\n---\n"} /name: koinon-agentd|component: agentd/ && /mountPath: \/secrets\/kek/{n++} END{print n+0}')
[[ $agentd_kek == 0 ]] || fail "koinon-agentd must not mount the KEKs with checkpoints on (ADR 0054)"
holders=$(render --show-only templates/deployments.yaml "${ckpt[@]}" | awk 'BEGIN{RS="\n---\n"} /AWS_SECRET_ACCESS_KEY/{n++} END{print n+0}')
[[ $holders == 1 ]] || fail "the S3 secret key is in exactly one Deployment (agentd), found $holders"
if render --set checkpoints.enabled=true --set checkpoints.s3.bucket=b >/dev/null 2>&1; then fail "checkpoints without s3.existingSecret must not render"; fi
if render --set checkpoints.enabled=true --set checkpoints.s3.existingSecret=s >/dev/null 2>&1; then fail "checkpoints without s3.bucket must not render"; fi
if render "${ckpt[@]}" --set checkpoints.s3.pathStyle=false >/dev/null 2>&1; then fail "checkpoints.s3.pathStyle=false must not render"; fi
