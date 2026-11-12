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
app_line=$(grep -nE '^        - CREATE ROLE koinon_app NOLOGIN$' <<<"$sql" | sed -n 1p | cut -d: -f1)
writer_line=$(grep -nE '^        - CREATE ROLE koinon_install_audit_writer NOLOGIN$' <<<"$sql" | sed -n 1p | cut -d: -f1)
if [[ -z $app_line || -z $writer_line || $writer_line -le $app_line ]]; then
	fail "koinon_install_audit_writer is created in postInitApplicationSQL after koinon_app"
fi

# Membership: koinon_api only.
has "koinon_api is a member of the writer role" '^        inRoles: \[koinon_app, koinon_install_audit_writer\]$' "${cluster[@]}"
# With recovery on (the default) koinon_reconciler is a member too, to append the recovery.<action> install
# events (ADR 0110 decision 6); with recovery off koinon_api is the only member.
n=$(grep -cE 'koinon_install_audit_writer\]' <<<"$sql" || true)
[[ $n == 2 ]] || fail "recovery on: koinon_api and koinon_reconciler are members of koinon_install_audit_writer (found $n)"
n=$(grep -cE '^        inRoles: \[koinon_app\]$' <<<"$sql" || true)
[[ $n == 1 ]] || fail "recovery on: koinon_agentd is a member of koinon_app only (found $n)"
off=$(render "${cluster[@]}" --set recovery.enabled=false)
n=$(grep -cE 'koinon_install_audit_writer\]' <<<"$off" || true)
[[ $n == 1 ]] || fail "recovery off: exactly one managed role is a member of koinon_install_audit_writer (found $n)"
n=$(grep -cE '^        inRoles: \[koinon_app\]$' <<<"$off" || true)
[[ $n == 2 ]] || fail "recovery off: koinon_reconciler and koinon_agentd are members of koinon_app only (found $n)"

# The member is the api role: the managed role list runs api, reconciler, agentd in that order.
api_role=$(grep -nE '^      - name: koinon_api$' <<<"$sql" | sed -n 1p | cut -d: -f1)
member_line=$(grep -nE 'inRoles: \[koinon_app, koinon_install_audit_writer\]' <<<"$sql" | sed -n 1p | cut -d: -f1)
[[ -n $api_role && -n $member_line && $((member_line - api_role)) -le 12 ]] ||
	fail "the writer role membership belongs to the koinon_api managed role"

# Clusters initialised before ADR 0100 never ran the new postInitApplicationSQL, and the migrate role
# (the initdb owner) has no CREATEROLE: the group role is also a managed role, without login.
has "the group role is a managed role without login" '^      - name: koinon_install_audit_writer$' "${cluster[@]}"
role_line=$(grep -nE '^      - name: koinon_install_audit_writer$' <<<"$sql" | sed -n 1p | cut -d: -f1)
block=$(sed -n "$((role_line + 1)),$((role_line + 3))p" <<<"$sql")
if ! grep -qE '^        ensure: present$' <<<"$block" || ! grep -qE '^        login: false$' <<<"$block"; then
	fail "the managed group role is ensure: present, login: false"
fi

# No non-CNPG render creates a role (the roles are the operator's, ADR 0100 https://koinon.stateracloud.com/docs/operate/secret-keys/).
lacks "an external database render has no CNPG group role" 'koinon_install_audit_writer' --set postgres.mode=external --set postgres.external.secretName=db

# The grant hook (install-audit-grants.yaml): on an upgrade of a cluster initialised before ADR 0100,
# CNPG creates the group role only after the pre-upgrade migrate Job, so a post-upgrade hook (Argo CD:
# PostSync) runs `koinon-api migrate --grants-only` as the migration role and waits for the role.
grants=(--show-only templates/install-audit-grants.yaml)
g=$(render "${grants[@]}")
for want in \
	'^    helm\.sh/hook: post-upgrade$' \
	'^    helm\.sh/hook-delete-policy: before-hook-creation,hook-succeeded$' \
	'^    argocd\.argoproj\.io/hook: PostSync$' \
	'^    argocd\.argoproj\.io/hook-delete-policy: BeforeHookCreation$' \
	'^          args: \["migrate", "--grants-only", "--wait", "4m"\]$' \
	'^ +name: koinon-db-migrate$' \
	'^      serviceAccountName: koinon-migrate-grants$' \
	'^      automountServiceAccountToken: false$' \
	'^          image: ghcr\.io/.*/api:'; do
	grep -Eq -- "$want" <<<"$g" || fail "the install audit grant hook renders $want"
done
# Not a post-install hook: a new cluster has the role before its first migration, and a post-install
# hook would make `helm install` without --wait wait for the new database.
grep -Eq 'helm\.sh/hook: .*post-install' <<<"$g" && fail "the install audit grant hook is not a post-install hook"
# Both the ServiceAccount and the Job are hooks of the same phases, the ServiceAccount first.
n=$(grep -cE '^    helm\.sh/hook: post-upgrade$' <<<"$g" || true)
[[ $n == 2 ]] || fail "the grant hook's ServiceAccount and Job are both post-upgrade hooks (found $n)"
n=$(grep -cE '^    argocd\.argoproj\.io/hook: PostSync$' <<<"$g" || true)
[[ $n == 2 ]] || fail "the grant hook's ServiceAccount and Job are both Argo CD PostSync hooks (found $n)"
grep -Eq '^    argocd\.argoproj\.io/sync-wave: "-10"$' <<<"$g" || fail "the grant hook's ServiceAccount comes before its Job in Argo CD"
# The pull secrets and the image registry follow the chart's helpers (ADR 0061).
has "the grant hook takes global.imagePullSecrets" '^        - name: regcred$' --set 'global.imagePullSecrets={regcred}' "${grants[@]}"
has "the grant hook takes global.imageRegistry" '^          image: mirror\.example/' --set global.imageRegistry=mirror.example "${grants[@]}"
# External mode: the operator creates the role before the upgrade; the pre-upgrade migrate grants it.
lacks "an external database render has no grant hook" 'migrate-grants' --set postgres.mode=external --set postgres.external.secretName=db

# The wait stays below Helm's default --timeout (5m), which bounds each hook: the grant hook then fails
# with its own message ("did not appear within 4m"), not a Helm timeout (docs upgrading.md).
has "migrations.wait defaults below Helm's default --timeout" '"--wait", "4m"' --show-only templates/migrate-job.yaml
# External mode: nothing creates the writer role after the migrate Job, so a missing role fails the Job
# with the remedy (koinon-api migrate --require-install-writer); in cnpg mode the pre-upgrade migrate only
# warns, because the Cluster creates the role after it and the post-upgrade hook grants it.
ext=(--set postgres.mode=external --set postgres.external.secretName=db --show-only templates/migrate-job.yaml)
has "the external-database migrate Job requires the writer role" '^          args: \["migrate", "--wait", "4m", "--require-install-writer"\]$' "${ext[@]}"
has "the external-database durable migrate Job requires the writer role" '^          args: \["migrate", "--wait", "4m", "--durable", "--require-install-writer"\]$' "${ext[@]}" --set api.durable.enabled=true
# koinon-api checks its own membership in the writer role (maintainer decision 2026-11-07): the chart
# has no value naming the api login and passes the migration no member to check.
lacks "the migrate Job checks no api login's membership" 'install-writer-member' "${ext[@]}" --set api.durable.enabled=true
lacks "the chart has no postgres.external.apiRole" 'apiRole' "${ext[@]}"
lacks "the cnpg migrate Job does not require the writer role" 'require-install-writer' --show-only templates/migrate-job.yaml
lacks "the grant hook does not require the writer role" 'require-install-writer' "${grants[@]}"
