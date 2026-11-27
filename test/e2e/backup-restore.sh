#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Backup, restore and verify on the install that test/e2e/run.sh just exercised (ADR 0085 decision 18,
# issue #90). It runs the blocks of test/e2e/commands/backup-restore.e1 (through hack/docs-e1.sh) as
# written; the guide on the docs site shows the same commands (the koinon-docs CI compares its page with
# the file, ADR 0108; hack/e2e-commands-check.sh checks this script against the file):
#
#   1. note the install: the install ID, the number of tasks and runs, the keys and what they wrap;
#   2. back up the keys and the database (the guide's backup blocks) and read the dump back;
#   3. change the install after the backup: add a marker table, and delete the KEK Secret (the guard refuses, then is set to Audit once);
#      with the gateway recipient key on, replace its Secret by a new install's key (as the key-admin service account);
#   4. stop, hold, restore the database, restore the keys, start held, resync (the guide's restore
#      blocks), check that nothing acts while held, verify the keys, release (issue #462);
#   5. verify: the marker is gone, the install ID, the task and run counts and the keys are the ones of
#      step 1, the KEK Secret is back, the key tool reports no warning, and the gateway key Secret is the
#      backup's and koinon-mcp-gateway is ready with it, at the replica count that stop noted (2 at least).
#
# Before step 1 and after step 5 it runs the restore specs of the Go driver (test/e2e/restore_test.go, issue
# #1335): before the backup, restore-seal seals a fake MCP credential to the gateway recipient key and calls a tool
# with it through koinon-mcp-gateway; after the restore, restore-open calls the tool again, so koinon-gateway must
# open the restored credential with the restored key, and a credential stored after the restore must be in envelope v2.
#
# It needs the kubectl wrapper that run.sh puts in $OUT/bin (pinned to the kind context), the Go driver that run.sh
# builds ($OUT/bin/e2e.test), the KOINON_E2E_* variables that run.sh passes (the admin's password and the fake MCP
# server's forward and CA), and nothing outside the kind cluster. The install must use gateway.mcp.driver=koinon (the
# default). Run by run.sh when BACKUP_RESTORE=1 (the nightly run, and a dispatch that asks for it); by hand against a
# cluster that run.sh left (KEEP=1): OUT=<run.sh's OUT> KOINON_E2E_...=<as run.sh sets them> test/e2e/backup-restore.sh

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
OUT=${OUT:?OUT is the directory run.sh logs to; its bin/kubectl pins the kind context}
NS=koinon
COMMANDS=$root/test/e2e/commands/backup-restore.e1
work="$OUT/backup-restore"
mkdir -p "$work"
# The key backups hold key material (test keys, but still keys): run.sh uploads $OUT as a CI artifact, so they
# do not stay after this script. The dump and the notes stay for diagnostics.
trap 'rm -f "$work/koinon-kek-backup.yaml" "$work/koinon-gateway-key-backup.yaml"' EXIT
K=("$OUT/bin/kubectl")
[[ -x ${K[0]} ]] || { echo "$OUT/bin/kubectl is missing: run test/e2e/run.sh first" >&2; exit 2; }
[[ -x $OUT/bin/e2e.test ]] || { echo "$OUT/bin/e2e.test is missing: run test/e2e/run.sh first (SUITE=go)" >&2; exit 2; }
for v in KOINON_E2E_API_URL KOINON_E2E_FAKEMCP_URL KOINON_E2E_FAKEMCP_CA KOINON_E2E_PASSWORD KOINON_E2E_NEW_PASSWORD; do
  [[ -n ${!v:-} ]] || { echo "$v is not set: run.sh passes it to the restore specs" >&2; exit 2; }
done

t_start=$(date +%s)
step() { echo "== $(date -u +%H:%M:%SZ) backup/restore: $*"; }
wait_a_bit() { command sleep "${1:-2}"; }
fail() { echo "backup/restore FAILED: $*" >&2; exit 1; }
# file_mode prints the octal mode of a file. GNU stat first, BSD stat (macOS) second, as in hack/agent-state.sh (#1474).
# hack/backup-restore-mode-test.sh extracts this line by its name.
file_mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

# blk runs one block of the command file, as the guide shows it, in the working directory that holds the
# dump and the key backup.
blk() {
  echo "-- guide block $1"
  E1_FILE=$COMMANDS E1_WORKDIR=$work PATH="$OUT/bin:$PATH" "$root/hack/docs-e1.sh" run "$1"
}

# spec runs one restore spec of the Go driver (label restore-seal or restore-open; test/e2e/restore_test.go).
spec() {
  echo "-- restore spec $1"
  (cd "$root/test/e2e" && KOINON_E2E_KUBECTL="${K[0]}" "$OUT/bin/e2e.test" -test.timeout 20m -ginkgo.v \
    -ginkgo.label-filter="$1" -ginkgo.junit-report="$work/$1-junit.xml") || fail "the restore spec $1 failed"
}

primary() { "${K[@]}" -n "$NS" get pod -l cnpg.io/cluster=koinon-pg,cnpg.io/instanceRole=primary -o name; }
sql() { "${K[@]}" -n "$NS" exec "$(primary)" -c postgres -- psql --dbname=koinon -v ON_ERROR_STOP=1 -Atq -c "$1"; }
keys_status() { "${K[@]}" -n "$NS" exec deploy/koinon-api -- koinon-api keys status; }

# state prints what a restore must bring back, one line per fact.
state() {
  echo "install_id=$(sql 'SELECT install_id FROM install_identity')"
  echo "tasks=$(sql 'SELECT count(*) FROM tasks')"
  echo "runs=$(sql 'SELECT count(*) FROM runs')"
  echo "migrations=$(sql 'SELECT max(version_id) FROM goose_db_version WHERE is_applied')"
  # The keys with the number of values each wraps (KEK, STATE, VALUES, ..., FINGERPRINT).
  keys_status | awk 'NF >= 5 && $2 ~ /^(active|retained|MISSING)$/ { print "kek=" $1, $2, $3, $NF }'
  # The stored MCP credentials by envelope: how many in v1, and how many in v2 under each gateway key.
  keys_status | sed -n 's/^MCP credentials: /mcp=/p'
}

step "state before the backup"
# A run that is still going can add rows between this read and pg_dump (and a recovered workflow can
# add some after the restart), so the counts below would not be what the dump holds. Require every
# run to be terminal first; a failure here means the driver left work in flight, not a restore bug.
inflight=$(sql "SELECT count(*) FROM runs WHERE state NOT IN ('succeeded', 'failed', 'canceled')")
[[ $inflight == 0 ]] || fail "$inflight run(s) are not in a terminal state: the counts would change between the read and the dump"
# A credential that the backup holds, sealed to the gateway key, and a tool call through koinon-gateway that opens it.
spec restore-seal
state | tee "$work/state.before"
[[ $(grep -c '^tasks=' "$work/state.before") == 1 ]] || fail "could not read the install's state"
tasks=$(sed -n 's/^tasks=//p' "$work/state.before")
(( tasks > 0 )) || fail "the install has no tasks: the quickstart driver should have left some, so a restore would prove nothing"
grep -q '^kek=' "$work/state.before" || fail "keys status lists no KEK"
grep -q '^mcp=.* in v2 under gateway key gk-' "$work/state.before" || fail "keys status lists no MCP credential in v2: restore-seal sealed one"

step "back up the keys and the database"
blk backup-keys
blk check-keys
blk note-keys
blk backup-gateway-key
blk backup-db
blk verify-dump
blk check-gateway-key
[[ -s $work/koinon.dump && -s $work/koinon-kek-backup.yaml ]] || fail "a backup file is empty"
# The gateway recipient key (gateway.mcp.driver=koinon only): its Secret is backed up next to the KEK, readable by
# its owner only. Compared by a digest of the data, so no key reaches this log.
GWKEY=koinon-gateway-recipient-key
# shellcheck disable=SC2016 # a go-template: $k and $v are template variables, not shell ones
gwdata='{{range $k, $v := .data}}{{$k}}={{$v}};{{end}}'
gwdigest() { "${K[@]}" -n "$NS" get secret "$GWKEY" -o go-template="$gwdata" | sha256sum; }
# The chart renders the key-admin ServiceAccount exactly when it expects the Secret (gateway.mcp.driver=koinon, the
# default, with recipientKey.enabled). Then the Secret is required: a missing one fails here, it is not skipped.
gwwant=0
if "${K[@]}" -n "$NS" get serviceaccount koinon-gateway-key-admin >/dev/null 2>&1; then
  gwwant=1
fi
gwon=0
if (( gwwant )) && ! "${K[@]}" -n "$NS" get secret "$GWKEY" >/dev/null 2>&1; then
  fail "the install uses the koinon MCP driver and has no gateway key Secret $GWKEY"
fi
if "${K[@]}" -n "$NS" get secret "$GWKEY" >/dev/null 2>&1; then
  gwon=1
  gwfile=$work/koinon-gateway-key-backup.yaml
  [[ -s $gwfile ]] || fail "the install has a gateway key Secret and backup-gateway-key wrote no file"
  gwmode=$(file_mode "$gwfile") || gwmode=
  [[ $gwmode == 600 ]] || fail "the gateway key backup is readable by others: ${gwmode:-unknown mode}"
  gw_before=$(gwdigest)
  backup_digest=$("${K[@]}" create -f "$gwfile" --dry-run=client --validate=false -o go-template="$gwdata" | sha256sum)
  [[ $backup_digest == "$gw_before" ]] || fail "the gateway key backup differs from the Secret"
elif [[ -e $work/koinon-gateway-key-backup.yaml ]]; then
  fail "backup-gateway-key wrote a file on an install without a gateway key Secret"
fi

step "change the install after the backup"
# Something the restore must undo (a table the dump does not have) and the loss it must repair
# (the KEK Secret: without it koinon-api refuses to start).
sql 'CREATE TABLE public.e2e_after_backup (note text)'
sql "INSERT INTO public.e2e_after_backup VALUES ('written after the backup')"
# The KEK Secret is guarded (issue #604): the admission policy refuses a delete. Prove that, then switch the
# binding to Audit for the one delete that simulates the loss, and back to Deny.
if "${K[@]}" -n "$NS" delete secret koinon-kek 2>"$work/kek-delete.err"; then
  fail "the KEK Secret was deleted: kek-guard is not enforced"
fi
grep -q 'cannot be deleted' "$work/kek-delete.err" || fail "the KEK delete was refused, but not by kek-guard: $(cat "$work/kek-delete.err")"
binding=$("${K[@]}" get validatingadmissionpolicybinding -o name | grep -- '-kek-guard$' || true)
[[ -n $binding ]] || fail "no kek-guard binding found"
setguard() {
  "${K[@]}" patch "$binding" --type=json -p "[{\"op\":\"replace\",\"path\":\"/spec/validationActions\",\"value\":[\"$1\"]}]" >/dev/null
}
setguard Audit
deleted=0
for _ in $(seq 1 30); do # the API server's admission cache needs a few seconds
  if "${K[@]}" -n "$NS" delete secret koinon-kek 2>/dev/null; then deleted=1; break; fi
  wait_a_bit
done
setguard Deny
(( deleted )) || fail "could not delete the KEK Secret with the guard in Audit"
# The usual restore is into a new install that made its own first KEK on its empty database, so the Secret
# exists and the backup is merged over it (keys import). koinon-api would not make a KEK here (the database
# holds stored secrets), so stand in for it with a fresh KEK of another ID. A create is not guarded.
fresh_id="kek-$(date -u +%Y%m%d)-e2e0"
"${K[@]}" -n "$NS" create secret generic koinon-kek --from-literal="$fresh_id=$(head -c 32 /dev/urandom | base64)" --from-literal=active="$fresh_id" >/dev/null \
  || fail "could not create the fresh KEK Secret"

if (( gwon )); then
  # The loss of the gateway key: a new install has its own first key, which no restored row names. The Secret is
  # guarded (gateway-key-guard): kubectl is refused, and only the key-admin service account writes it.
  if "${K[@]}" -n "$NS" delete secret "$GWKEY" 2>"$work/gwkey-delete.err"; then
    fail "the gateway key Secret was deleted: gateway-key-guard is not enforced"
  fi
  grep -q 'only the gateway key-admin Job may write' "$work/gwkey-delete.err" || fail "the gateway key delete was refused, but not by gateway-key-guard: $(cat "$work/gwkey-delete.err")"
  gwadmin=system:serviceaccount:$NS:koinon-gateway-key-admin
  "${K[@]}" -n "$NS" delete secret "$GWKEY" --as="$gwadmin" >/dev/null || fail "the key-admin service account could not delete the gateway key Secret"
  "${K[@]}" -n "$NS" create secret generic "$GWKEY" --as="$gwadmin" --from-literal=gk-00000000000000e2=koinon_test_not-a-real-token >/dev/null ||
    fail "could not stand in for a new install's gateway key Secret"
fi
step "restore"
t_restore=$(date +%s)
blk stop
blk hold
blk restore-db
blk restore-keys
blk restore-gateway-key
blk start
blk resync
# Held: every component says so, and the schedule and run machinery is off. The restored install has a
# due-schedule and run backlog that must not move before the release.
for c in koinon-api koinon-reconciler koinon-agentd; do
  logs=$("${K[@]}" -n "$NS" logs "deploy/$c")
  grep -q 'restore-hold' <<<"$logs" || fail "$c did not log that it is in restore-hold mode"
done
api_logs=$("${K[@]}" -n "$NS" logs deploy/koinon-api)
if grep -q 'DBOS runtime started' <<<"$api_logs"; then
  fail "koinon-api started the DBOS runtime while held"
fi
claimed=$(sql "SELECT count(*) FROM runs WHERE owner_id IS NOT NULL AND state IN ('starting', 'running') AND lease_expires_at > now()")
[[ $claimed == 0 ]] || fail "$claimed run(s) hold a live lease while the install is held"
blk compare-keys
blk verify-keys
blk verify-gateway-key
blk release
for c in koinon-api koinon-reconciler koinon-agentd; do
  envs=$("${K[@]}" -n "$NS" get "deploy/$c" -o jsonpath='{.spec.template.spec.containers[0].env[*].name}')
  if grep -q 'KOINON_RESTORE_HOLD' <<<"$envs"; then
    fail "$c still has KOINON_RESTORE_HOLD after the release"
  fi
done
t_done=$(date +%s)

step "verify"
[[ $(sql "SELECT coalesce(to_regclass('public.e2e_after_backup')::text, 'gone')") == gone ]] ||
  fail "the table created after the backup survived the restore"
"${K[@]}" -n "$NS" get secret koinon-kek >/dev/null || fail "the KEK Secret is not back"
# The guard is back: kubectl cannot change the Secret, and the guide's import (koinon-api's own identity) can.
wait_a_bit 5
if "${K[@]}" -n "$NS" annotate secret koinon-kek e2e-probe=1 --overwrite 2>/dev/null; then
  fail "kubectl changed the KEK Secret: kek-guard is not enforced after the restore"
fi
"${K[@]}" -n "$NS" exec -i deploy/koinon-api -- koinon-api keys import --yes <"$work/koinon-kek-backup.yaml" ||
  fail "koinon-api keys import failed over the existing KEK Secret"
# The merge keeps the fresh KEK the restore went over (an import removes none); it wraps no value.
state | grep -v "^kek=$fresh_id " | tee "$work/state.after"
diff -u "$work/state.before" "$work/state.after" || fail "the restored install differs from the one that was backed up"
ks=$(keys_status)
if grep -qiE 'warning|missing' <<<"$ks"; then
  printf '%s\n' "$ks" >&2
  fail "keys status warns after the restore"
fi
if (( gwon )); then
  [[ $(gwdigest) == "$gw_before" ]] || fail "the gateway key Secret is not the one that was backed up"
  if "${K[@]}" -n "$NS" annotate secret "$GWKEY" e2e-probe=1 --overwrite 2>/dev/null; then
    fail "kubectl changed the gateway key Secret: gateway-key-guard is not enforced after the restore"
  fi
  "${K[@]}" -n "$NS" rollout status deploy/koinon-mcp-gateway --timeout=2m || fail "koinon-mcp-gateway is not ready with the restored gateway keys"
fi
# start brought each Deployment back to the replica count that stop noted. koinon-mcp-gateway runs 2 copies at least
# (the chart refuses fewer), and the MCP credential format gate counts them.
[[ -s $work/koinon-replicas.txt ]] || fail "stop noted no replica counts"
while read -r d n; do
  [[ $d == mcp-gateway ]] && { (( n >= 2 )) || fail "stop noted $n koinon-mcp-gateway replica(s): the chart runs 2 at least"; }
  got=$("${K[@]}" -n "$NS" get "deploy/koinon-$d" -o jsonpath='{.spec.replicas}')
  [[ $got == "$n" ]] || fail "koinon-$d runs $got replica(s) after the restore, stop noted $n"
  ready=$("${K[@]}" -n "$NS" get "deploy/koinon-$d" -o jsonpath='{.status.readyReplicas}')
  [[ ${ready:-0} == "$n" ]] || fail "koinon-$d has ${ready:-0} ready replica(s) after the restore, stop noted $n"
done <"$work/koinon-replicas.txt"
grep -q '^mcp-gateway ' "$work/koinon-replicas.txt" || fail "stop noted no koinon-mcp-gateway: the drill needs gateway.mcp.driver=koinon"
# The migration role must still own its schema: this is what the next helm upgrade Job runs as.
sql "SET ROLE koinon_migrate; CREATE TABLE public.e2e_migrate_probe (); DROP TABLE public.e2e_migrate_probe" >/dev/null
# koinon-api is Ready only with the restored keys and a schema it can read (the start block waited).
"${K[@]}" -n "$NS" get deploy koinon-api koinon-reconciler koinon-agentd
# The restored credential opens with the restored gateway key on a tool call, and a new one is sealed in v2.
spec restore-open

{
  echo "Backup and restore (test/e2e/commands/backup-restore.e1 blocks, kind, cnpg): restore took $(( t_done - t_restore )) s, whole step $(( t_done - t_start )) s."
} | tee "$work/timings.md"
echo "backup/restore passed"
