#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/audit-batch-lint.sh offline with fixture trees in a temp dir: each planted bypass must
# fail the lint, clean code and the allowed paths must pass, and the real tree is clean.

# shellcheck disable=SC2016 # the fixtures are Go and SQL text with $ and backticks, not shell

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
script="$repo/hack/audit-batch-lint.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/audit-batch-lint-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
fail=0
ok() { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

n=0
# case_ <name> <want rc> <path> <content>: a tree with one file.
case_() {
	n=$((n + 1))
	local r=$tmp/r$n rc
	mkdir -p "$r/$(dirname "$3")"
	printf '%s\n' "$4" >"$r/$3"
	"$script" "$r" >/dev/null 2>&1
	rc=$?
	if [[ $rc -eq $2 ]]; then ok "$1"; else bad "$1 (rc $rc, want $2)"; fi
}

case_ "a batch write passes" 0 internal/foo/a.go 'package foo
func f() { _, _ = audit.InTx(ctx, db, func(q *store.Queries, b audit.Batch) error { b.Org(e); return nil }) }'
case_ "RecordAfter passes" 0 internal/foo/a.go 'package foo
func f() { _ = audit.RecordAfter(ctx, q, e) }'
case_ "a method named Record on another type passes" 0 internal/foo/a.go 'package foo
func f() { _ = a.Record(ctx, e) }'

case_ "raw INSERT INTO audit_events fails" 1 internal/foo/a.go 'package foo
var s = `INSERT INTO audit_events (org_id) VALUES ($1)`'
case_ "raw INSERT with the table on the next line fails" 1 internal/foo/a_test.go 'package foo
var s = `INSERT INTO
	install_audit_events (actor_kind) VALUES ($1)`'
case_ "raw UPDATE of install_audit_events fails" 1 internal/foo/a_test.go 'package foo
var s = `update install_audit_events set actor_id = $1`'
case_ "raw DELETE FROM audit_chain_heads fails" 1 internal/foo/a_test.go 'package foo
var s = `DELETE FROM audit_chain_heads`'
case_ "raw TRUNCATE install_audit_chain_head fails" 1 internal/foo/a_test.go 'package foo
var s = `TRUNCATE TABLE install_audit_chain_head`'
case_ "raw INSERT INTO install_audit_anchor_heads fails" 1 internal/foo/a.go 'package foo
var s = `INSERT INTO install_audit_anchor_heads (anchor_id) VALUES ($1)`'
case_ "a SELECT of an audit table passes" 0 internal/foo/a.go 'package foo
var s = `SELECT count(*) FROM audit_events`'
case_ "an insert into another table passes" 0 internal/foo/a.go 'package foo
var s = `INSERT INTO agents (id) VALUES ($1)`'
case_ "a sqlc query that writes an audit table fails" 1 internal/db/queries/agents.sql '-- name: Bad :exec
DELETE FROM audit_events WHERE org_id = $1;'
case_ "an insert in the store's audit.sql fails (the inserts live in auditstore)" 1 internal/db/queries/audit.sql '-- name: InsertAuditEvent :one
INSERT INTO audit_events (org_id) VALUES ($1) RETURNING *;'
case_ "the auditstore queries pass" 0 internal/audit/internal/auditstore/queries/audit_write.sql '-- name: InsertAuditEvent :one
INSERT INTO audit_events (org_id) VALUES ($1) RETURNING *;'
case_ "a once-allowlisted test path with no marker fails" 1 internal/cmd/koinonapi/audit_test.go 'package main
var s = `UPDATE install_audit_events SET target_id = $1`'
case_ "internal/api/audit_test.go is no longer allowlisted" 1 internal/api/audit_test.go 'package api_test
var s = `INSERT INTO install_audit_events (actor_kind) VALUES ($1)`'
case_ "a raw write inside internal/audit passes" 0 internal/audit/x_test.go 'package audit_test
var s = `DELETE FROM audit_events`'

case_ "audit.WriteInTx outside keyscli fails" 1 internal/foo/a.go 'package foo
func f() { _, _ = audit.WriteInTx(ctx, tx, fn) }'
case_ "audit.WriteInTx in a test outside keyscli fails" 1 internal/foo/a_test.go 'package foo
func f() { _, _ = audit.WriteInTx(ctx, tx, fn) }'
case_ "audit.WriteInTx in keyscli passes" 0 internal/keyscli/a.go 'package keyscli
func f() { _, _ = audit.WriteInTx(ctx, tx, fn) }'
case_ "audit.WriteInTx in internal/audit passes" 0 internal/audit/batch.go 'package audit
func f() { _, _ = WriteInTx(ctx, tx, fn) }'
case_ "an aliased WriteInTx fails" 1 internal/foo/a.go 'package foo
import au "github.com/stateracloud/koinon/internal/audit"
func f() { _, _ = au.WriteInTx(ctx, tx, fn) }'
case_ "a dot-imported WriteInTx fails" 1 internal/foo/a.go 'package foo
import . "github.com/stateracloud/koinon/internal/audit"
func f() { _, _ = WriteInTx (ctx, tx, fn) }'
case_ "an aliased WriteInTx in keyscli passes" 0 internal/keyscli/a.go 'package keyscli
import au "github.com/stateracloud/koinon/internal/audit"
func f() { _, _ = au.WriteInTx(ctx, tx, fn) }'

# Rule 1: schema-qualified and quoted names, COPY, MERGE; rule 2: pgx.Identifier.
case_ "INSERT INTO public.audit_events fails" 1 internal/foo/a.go 'package foo
var s = `INSERT INTO public.audit_events (org_id) VALUES ($1)`'
case_ "INSERT INTO a quoted table fails" 1 internal/foo/a.go 'package foo
var s = `INSERT INTO "audit_events" (org_id) VALUES ($1)`'
case_ "UPDATE public.\"audit_chain_heads\" fails" 1 internal/foo/a.go 'package foo
var s = `UPDATE public."audit_chain_heads" SET seq = 0`'
case_ "a quoted table in a Go string fails" 1 internal/foo/a.go 'package foo
var s = "DELETE FROM \"public\".\"install_audit_events\""'
case_ "COPY into an audit table fails" 1 internal/foo/a.go 'package foo
var s = `COPY install_audit_events (actor_kind) FROM STDIN`'
case_ "MERGE INTO an audit table fails" 1 internal/foo/a.go 'package foo
var s = `MERGE INTO public.audit_events t USING src s ON false WHEN NOT MATCHED THEN INSERT DEFAULT VALUES`'
case_ "a sqlc query with MERGE INTO fails" 1 internal/db/queries/agents.sql '-- name: Bad :exec
MERGE INTO "audit_events" USING x ON false WHEN NOT MATCHED THEN DO NOTHING;'
case_ "CopyFrom with pgx.Identifier fails" 1 internal/foo/a.go 'package foo
func f() { _, _ = conn.CopyFrom(ctx, pgx.Identifier{"audit_events"}, cols, src) }'
case_ "CopyFrom with a schema-qualified Identifier fails" 1 internal/foo/a_test.go 'package foo
func f() { _, _ = tx.CopyFrom(ctx, pgx.Identifier{"public", "install_audit_chain_head"}, cols, src) }'
case_ "a pgx.Identifier of another table passes" 0 internal/foo/a.go 'package foo
func f() { _, _ = conn.CopyFrom(ctx, pgx.Identifier{"public", "agents"}, cols, src) }'
case_ "a SELECT of public.audit_events passes" 0 internal/foo/a.go 'package foo
var s = `SELECT count(*) FROM public."audit_events"`'
case_ "an allowlisted tamper test with a qualified name passes" 0 internal/db/migrations/audit_test.go 'package migrations
var s = `UPDATE public.audit_events SET action = $1`'

# Line markers: // audit-lint:allow <reason> on the line directly above the reported line, in a test.
case_ "a marker with a reason passes" 0 internal/foo/a_test.go 'package foo
func f() {
	// audit-lint:allow a tamper test: forges a row so the verifier has a break to report
	_, _ = tx.Exec(ctx, `UPDATE install_audit_events SET target_id = $1`)
}'
case_ "a marker above a multi-line statement passes" 0 internal/foo/a_test.go 'package foo
func f() {
	// audit-lint:allow tests the database grants of the writer role
	_, _ = c.Exec(ctx, `INSERT INTO install_audit_events (actor_kind)
		VALUES ($1)`)
}'
case_ "a marker without a reason fails" 1 internal/foo/a_test.go 'package foo
func f() {
	// audit-lint:allow
	_, _ = tx.Exec(ctx, `UPDATE install_audit_events SET target_id = $1`)
}'
case_ "a marker with only blanks as a reason fails" 1 internal/foo/a_test.go 'package foo
func f() {
	// audit-lint:allow   
	_, _ = tx.Exec(ctx, `UPDATE install_audit_events SET target_id = $1`)
}'
case_ "a marker that is not adjacent fails" 1 internal/foo/a_test.go 'package foo
func f() {
	// audit-lint:allow a tamper test
	x := 1

	_, _ = tx.Exec(ctx, `UPDATE install_audit_events SET target_id = $1`)
}'
case_ "a marker on the same line as the statement fails" 1 internal/foo/a_test.go 'package foo
func f() {
	_, _ = tx.Exec(ctx, `UPDATE install_audit_events SET target_id = $1`) // audit-lint:allow a tamper test
}'
case_ "a marker outside a test fails" 1 internal/foo/a.go 'package foo
func f() {
	// audit-lint:allow not a test
	_, _ = tx.Exec(ctx, `UPDATE install_audit_events SET target_id = $1`)
}'
case_ "a marker above a line with no audit write fails" 1 internal/foo/a_test.go 'package foo
func f() {
	// audit-lint:allow left behind
	x := 1
}'
case_ "a marker covers the next line only" 1 internal/foo/a_test.go 'package foo
func f() {
	// audit-lint:allow a tamper test
	_, _ = tx.Exec(ctx, `UPDATE install_audit_events SET target_id = $1`)
	_, _ = tx.Exec(ctx, `DELETE FROM audit_events`)
}'
case_ "a marker does not excuse WriteInTx" 1 internal/foo/a_test.go 'package foo
func f() {
	// audit-lint:allow not for this rule
	_, _ = audit.WriteInTx(ctx, tx, fn)
}'

# Rule 2: a whole string literal that is an audit table name (dynamic SQL).
case_ "a concatenated INSERT INTO with the table as a literal fails" 1 internal/foo/a.go 'package foo
func f() { t := "audit_events"; _, _ = db.Exec(ctx, "INSERT INTO "+t+" (org_id) VALUES ($1)", org) }'
case_ "a Sprintf with the table as an argument fails" 1 internal/foo/a.go 'package foo
func f() { _, _ = db.Exec(ctx, fmt.Sprintf("DELETE FROM %s", "install_audit_events")) }'
case_ "a schema-qualified literal fails" 1 internal/foo/a.go 'package foo
var t = "public.audit_chain_heads"'
case_ "a raw-string literal fails" 1 internal/foo/a_test.go 'package foo
var t = `install_audit_chain_head`'
case_ "a quoted identifier as a literal fails" 1 internal/foo/a.go 'package foo
var t = `"public"."install_audit_events"`'
case_ "a longer string that contains the name does not trigger rule 2" 0 internal/foo/a.go 'package foo
var a = "SELECT count(*) FROM audit_events"
var b = "audit_events_total"
var c = "the audit_events table"'
case_ "a name that is not an audit table passes" 0 internal/foo/a.go 'package foo
var t = "audit_chain_head"'
case_ "a marked table literal in a test passes" 0 internal/foo/a_test.go 'package foo
func f() {
	// audit-lint:allow tests the database grants on the table
	_, _ = conn.CopyFrom(ctx, pgx.Identifier{"install_audit_events"}, cols, src)
}'
case_ "a table literal inside internal/audit passes" 0 internal/audit/a.go 'package audit
var t = "audit_events"'
case_ "a table literal in an allowlisted migration test passes" 0 internal/db/migrations/audit_chain_test.go 'package migrations
var t = "audit_chain_heads"'

# A git checkout: untracked files that git does not ignore are scanned, ignored ones and deleted
# tracked files are skipped.
g=$tmp/git
mkdir -p "$g/internal/foo" "$g/internal/gone" "$g/internal/ign"
git -C "$g" init -q
printf 'package foo\n' >"$g/internal/foo/ok.go"
printf 'package gone\nvar s = `DELETE FROM audit_events`\n' >"$g/internal/gone/gone.go"
printf 'internal/ign/\n' >"$g/.gitignore"
git -C "$g" add -A
git -C "$g" -c user.name=t -c user.email=t@example.invalid commit -qm init
rm "$g/internal/gone/gone.go"
printf 'package ign\nvar s = `DELETE FROM audit_events`\n' >"$g/internal/ign/ign.go"
if "$script" "$g" >/dev/null 2>&1; then ok "git: a deleted tracked file and an ignored file are skipped"; else bad "git: a deleted tracked file or an ignored file was scanned"; fi
printf 'package foo\nvar s = `DELETE FROM audit_events`\n' >"$g/internal/foo/new.go"
if "$script" "$g" >/dev/null 2>&1; then bad "git: an untracked new file was not scanned"; else ok "git: an untracked new file fails"; fi
report=$("$script" "$g" 2>&1)
if grep -q '^internal/foo/new.go:2: SQL write' <<<"$report"; then ok "git: the report names path:line"; else bad "git: the report does not name internal/foo/new.go:2"; fi

# The real tree is clean.
if "$script" >/dev/null 2>&1; then ok "repository is clean"; else bad "repository writes an audit table outside internal/audit"; fi

exit "$fail"
