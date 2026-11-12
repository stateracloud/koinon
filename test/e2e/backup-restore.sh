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
#   4. stop, hold, restore the database, restore the keys, start held, resync (the guide's restore
#      blocks), check that nothing acts while held, verify the keys, release (issue #462);
#   5. verify: the marker is gone, the install ID, the task and run counts and the keys are the ones of
#      step 1, the KEK Secret is back, and the key tool reports no warning.
#
# It needs the kubectl wrapper that run.sh puts in $OUT/bin (pinned to the kind context), and nothing
# outside the kind cluster. Run by run.sh when BACKUP_RESTORE=1 (the nightly run, and a dispatch that
# asks for it); by hand against a cluster that run.sh left (KEEP=1): OUT=<run.sh's OUT> test/e2e/backup-restore.sh

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
OUT=${OUT:?OUT is the directory run.sh logs to; its bin/kubectl pins the kind context}
NS=koinon
COMMANDS=$root/test/e2e/commands/backup-restore.e1
work="$OUT/backup-restore"
mkdir -p "$work"
K=("$OUT/bin/kubectl")
[[ -x ${K[0]} ]] || { echo "$OUT/bin/kubectl is missing: run test/e2e/run.sh first" >&2; exit 2; }

t_start=$(date +%s)
step() { echo "== $(date -u +%H:%M:%SZ) backup/restore: $*"; }
wait_a_bit() { command sleep "${1:-2}"; }
fail() { echo "backup/restore FAILED: $*" >&2; exit 1; }

# blk runs one block of the command file, as the guide shows it, in the working directory that holds the
# dump and the key backup.
blk() {
  echo "-- guide block $1"
  E1_FILE=$COMMANDS E1_WORKDIR=$work PATH="$OUT/bin:$PATH" "$root/hack/docs-e1.sh" run "$1"
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
}

step "state before the backup"
# A run that is still going can add rows between this read and pg_dump (and a recovered workflow can
# add some after the restart), so the counts below would not be what the dump holds. Require every
# run to be terminal first; a failure here means the driver left work in flight, not a restore bug.
inflight=$(sql "SELECT count(*) FROM runs WHERE state NOT IN ('succeeded', 'failed', 'canceled')")
[[ $inflight == 0 ]] || fail "$inflight run(s) are not in a terminal state: the counts would change between the read and the dump"
state | tee "$work/state.before"
[[ $(grep -c '^tasks=' "$work/state.before") == 1 ]] || fail "could not read the install's state"
tasks=$(sed -n 's/^tasks=//p' "$work/state.before")
(( tasks > 0 )) || fail "the install has no tasks: the quickstart driver should have left some, so a restore would prove nothing"
grep -q '^kek=' "$work/state.before" || fail "keys status lists no KEK"

step "back up the keys and the database"
blk backup-keys
blk check-keys
blk backup-db
blk verify-dump
[[ -s $work/koinon.dump && -s $work/koinon-kek-backup.yaml ]] || fail "a backup file is empty"

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

step "restore"
t_restore=$(date +%s)
blk stop
blk hold
blk restore-db
blk restore-keys
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
blk verify-keys
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
# The migration role must still own its schema: this is what the next helm upgrade Job runs as.
sql "SET ROLE koinon_migrate; CREATE TABLE public.e2e_migrate_probe (); DROP TABLE public.e2e_migrate_probe" >/dev/null
# koinon-api is Ready only with the restored keys and a schema it can read (the start block waited).
"${K[@]}" -n "$NS" get deploy koinon-api koinon-reconciler koinon-agentd

{
  echo "Backup and restore (test/e2e/commands/backup-restore.e1 blocks, kind, cnpg): restore took $(( t_done - t_restore )) s, whole step $(( t_done - t_start )) s."
} | tee "$work/timings.md"
echo "backup/restore passed"
