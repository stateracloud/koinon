# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash
#
# Credentials for hack/e2e-argo.sh on the maintainers' home lab (sourced, not run). They come from the
# lab's OpenBao with the AppRole in ~/.config/koinon/openbao (role_id, secret_id files), which only
# the maintainers have. Other Argo installations: write a file with the same four functions and set
# KOINON_E2E_ARGO_CREDS. Values stay in shell variables of the calling script; nothing is written.
#
# The Argo token is a short-lived ServiceAccount token issued under the OpenBao login, so the login
# stays alive until the run ends (revoking it revokes the token): e2e_creds_close runs only then.

: "${BAO_ADDR:=https://bao.apps.lab.example.invalid}"
export BAO_ADDR
_e2e_approle=${KOINON_OPENBAO_APPROLE_DIR:-$HOME/.config/koinon/openbao}
_e2e_bao=""

e2e_creds_open() {
  command -v bao >/dev/null || { echo "e2e-remote --argo: needs the bao CLI (OpenBao)" >&2; return 1; }
  [[ -r $_e2e_approle/role_id && -r $_e2e_approle/secret_id ]] ||
    { echo "e2e-remote --argo: no AppRole in $_e2e_approle (maintainers only; see --local)" >&2; return 1; }
  _e2e_bao=$(bao write -field=token auth/approle/login \
    role_id=@"$_e2e_approle/role_id" secret_id=@"$_e2e_approle/secret_id")
  # Long enough for a queued run: the issued token dies with this login.
  BAO_TOKEN=$_e2e_bao bao token renew -increment=3h >/dev/null 2>&1 || true
}

e2e_creds_argo_token() {
  BAO_TOKEN=$_e2e_bao bao write -f -format=json kubernetes-koinon/creds/koinon-e2e-submitter \
    kubernetes_namespace=koinon-e2e ttl=3h | jq -r '.data.service_account_token // empty'
}

e2e_creds_s3() {
  local id key
  id=$(BAO_TOKEN=$_e2e_bao bao kv get -field=AWS_ACCESS_KEY_ID koinon/shared/e2e-bundles-s3)
  key=$(BAO_TOKEN=$_e2e_bao bao kv get -field=AWS_SECRET_ACCESS_KEY koinon/shared/e2e-bundles-s3)
  printf '%s:%s\n' "$id" "$key"
}

# A read-only key on the results bucket (e2e-artifacts).
e2e_creds_results() {
  local id key
  id=$(BAO_TOKEN=$_e2e_bao bao kv get -field=AWS_ACCESS_KEY_ID koinon/shared/e2e-artifacts-reader)
  key=$(BAO_TOKEN=$_e2e_bao bao kv get -field=AWS_SECRET_ACCESS_KEY koinon/shared/e2e-artifacts-reader)
  printf '%s:%s\n' "$id" "$key"
}

e2e_creds_close() {
  [[ -z $_e2e_bao ]] || BAO_TOKEN=$_e2e_bao bao token revoke -self >/dev/null 2>&1 || true
  _e2e_bao=""
}
