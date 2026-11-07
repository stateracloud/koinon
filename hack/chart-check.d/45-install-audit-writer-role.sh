# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the install audit writer group role (ADR 0100 decision 1.1a, #328).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).
# Only koinon-api writes the install audit chain. The chart creates the NOLOGIN group role before
# any migration runs and only koinon_api's managed role is a member: a login role CNPG creates
# asynchronously cannot be named in the migration's GRANT, a group role can.

cluster=(--show-only templates/postgres-cnpg.yaml)

has "the group role is created in postInitApplicationSQL" '^        - CREATE ROLE koinon_install_audit_writer NOLOGIN$' "${cluster[@]}"

# The group role is created after koinon_app and before the managed roles can join it: both are
# bootstrap statements, in this order.
sql=$(render "${cluster[@]}")
app_line=$(grep -nE '^        - CREATE ROLE koinon_app NOLOGIN$' <<<"$sql" | head -1 | cut -d: -f1)
writer_line=$(grep -nE '^        - CREATE ROLE koinon_install_audit_writer NOLOGIN$' <<<"$sql" | head -1 | cut -d: -f1)
if [[ -z $app_line || -z $writer_line || $writer_line -le $app_line ]]; then
	fail "koinon_install_audit_writer is created in postInitApplicationSQL after koinon_app"
fi

# Membership: koinon_api only.
has "koinon_api is a member of the writer role" '^        inRoles: \[koinon_app, koinon_install_audit_writer\]$' "${cluster[@]}"
n=$(grep -cE 'koinon_install_audit_writer\]' <<<"$sql" || true)
[[ $n == 1 ]] || fail "exactly one managed role is a member of koinon_install_audit_writer (found $n)"
n=$(grep -cE '^        inRoles: \[koinon_app\]$' <<<"$sql" || true)
[[ $n == 2 ]] || fail "koinon_reconciler and koinon_agentd are members of koinon_app only (found $n)"

# The member is the api role: the managed role list runs api, reconciler, agentd in that order.
api_role=$(grep -nE '^      - name: koinon_api$' <<<"$sql" | head -1 | cut -d: -f1)
member_line=$(grep -nE 'inRoles: \[koinon_app, koinon_install_audit_writer\]' <<<"$sql" | head -1 | cut -d: -f1)
[[ -n $api_role && -n $member_line && $((member_line - api_role)) -le 5 ]] ||
	fail "the writer role membership belongs to the koinon_api managed role"

# Clusters initialised before ADR 0100 never ran the new postInitApplicationSQL, and the migrate role
# (the initdb owner) has no CREATEROLE: the group role is also a managed role, without login.
has "the group role is a managed role without login" '^      - name: koinon_install_audit_writer$' "${cluster[@]}"
role_line=$(grep -nE '^      - name: koinon_install_audit_writer$' <<<"$sql" | head -1 | cut -d: -f1)
block=$(sed -n "$((role_line + 1)),$((role_line + 3))p" <<<"$sql")
if ! grep -qE '^        ensure: present$' <<<"$block" || ! grep -qE '^        login: false$' <<<"$block"; then
	fail "the managed group role is ensure: present, login: false"
fi

# No non-CNPG render creates a role (the roles are the operator's, ADR 0100 docs/ops/secret-keys.md).
lacks "an external database render has no CNPG group role" 'koinon_install_audit_writer' --set postgres.mode=external --set postgres.external.secretName=db
