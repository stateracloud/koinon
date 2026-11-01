# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash
#
# Template for the credentials file of hack/e2e-argo.sh (sourced, not run; name your copy in
# KOINON_E2E_ARGO_CREDS). Keep your copy outside the repository. Fetch the values from your own
# secret store; they must stay in shell variables of the calling script and are never written or
# put in argv. The functions below only mark the contract.

e2e_creds_open() { :; } # log in to your secret store; called once, before the first use

e2e_creds_argo_token() { echo "bearer token for the Argo Server"; }

# ACCESS_KEY_ID:SECRET_KEY of a key that may write the bundle bucket.
e2e_creds_s3() { echo "koinon_test_example-id:koinon_test_example-secret"; }

# Optional: the same for a read-only key on the results bucket (else e2e_creds_s3 is used).
e2e_creds_results() { echo "koinon_test_example-id:koinon_test_example-secret"; }

e2e_creds_close() { :; } # log out; called when a followed run ends
