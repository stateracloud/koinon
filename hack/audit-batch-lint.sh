#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Audit batch lint (#371, ADR 0100 decision 1.7 and its audit batch amendment): the audit batch
# (audit.InTx, audit.InPgTx) and the named after-the-fact entry point (audit.RecordAfter) are the only
# ways to write an audit event, so the module alone decides the lock order of the audit heads. Fails
# when Go code, tests included, does any of this outside internal/audit:
#   1. calls audit.Record, audit.RecordErr, audit.RecordInstall or audit.RecordInstallErr (the direct
#      writes step 3 deleted; this keeps them from coming back under the same name);
#   2. calls the sqlc insert queries of the audit tables (InsertAuditEvent, InsertInstallAuditEvent);
#   3. writes the audit tables with SQL (INSERT, UPDATE, DELETE, TRUNCATE on audit_events,
#      audit_chain_heads, install_audit_events, install_audit_chain_head), in a Go file or in a sqlc
#      query file other than internal/db/queries/audit.sql;
#   4. calls audit.WriteInTx anywhere but internal/keyscli (the one caller whose transaction outlives
#      a function, see the doc comment of WriteInTx).
# A test that must break the tables on purpose (a tamper test, a test of the database's own grants) is
# allowed by path, listed in SQL_ALLOW below with the reason. Add a path there only for a test that
# proves the database refuses or detects a write the module would never make.
#
# Scans tracked files under the root; with a root that is not a git checkout it scans every file.
#
# Usage: hack/audit-batch-lint.sh [root]    (default: the repository root)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=${1:-$(cd "$(dirname "$0")/.." && pwd)}
cd "$root"

# Code that owns the audit tables: the module itself and the sqlc output of its queries.
OWNER_RE='^internal/(audit|db/store)/'
# Rule 4: the one allowed user of audit.WriteInTx.
WRITEINTX_RE='^internal/keyscli/'
# Rule 3 allowlist, by path. Each of these tests writes an audit table directly on purpose.
SQL_ALLOW_RE='^(internal/db/migrations/audit(_chain)?_test\.go|internal/api/audit_test\.go|internal/cmd/koinonapi/audit_test\.go|test/chartsmoke/smoke_test\.go)$'
#   internal/db/migrations/audit*_test.go test the tables' own triggers, grants and hash chain with raw SQL
#   internal/api/audit_test.go            inserts an install event as the database role under test, to show the
#                                         database accepts the writer role and refuses another
#   internal/cmd/koinonapi/audit_test.go tampers with a row (session_replication_role = replica) so
#                                         "audit verify" has a break to report
#   test/chartsmoke/smoke_test.go         inserts through the chart's install audit writer role, to show the
#                                         rendered grants work

if git rev-parse --is-inside-work-tree >/dev/null 2>&1 && [[ $(git rev-parse --show-toplevel) == "$(pwd -P)" ]]; then
	files=$(git ls-files -- '*.go' 'internal/db/queries/*.sql')
else
	files=$(find . \( -name '*.go' -o -path '*/internal/db/queries/*.sql' \) -type f | sed 's|^\./||')
fi

out=$(mktemp "${TMPDIR:-/tmp}/audit-batch-lint.XXXXXX") || exit 1
trap 'rm -f "$out"' EXIT

# scan <file> <rule text> <perl regexp>: report every match of the regexp (case-insensitive, across lines).
scan() {
	local f=$1 why=$2 re=$3
	WHY=$why RE=$re F=$f perl -0777 -ne '
		my $re = qr/$ENV{RE}/i;
		while ($_ =~ /$re/g) { my $n = (substr($_, 0, $-[0]) =~ tr/\n//) + 1; print "$ENV{F}:$n: $ENV{WHY}\n"; }
	' <"$f" >>"$out" || true
}

n=0
while IFS= read -r f; do
	[[ -n $f && -f $f ]] || continue
	n=$((n + 1))
	[[ $f =~ $OWNER_RE ]] && continue
	if [[ $f == *.go ]]; then
		scan "$f" "direct audit write (use audit.InTx/InPgTx, or audit.RecordAfter for an after-the-fact event)" '\baudit\.Record(Err|Install|InstallErr)?\b'
		scan "$f" "store insert of an audit table outside internal/audit (add the event to an audit batch)" '\b(InsertAuditEvent|InsertInstallAuditEvent)(Params)?\b'
		[[ $f =~ $WRITEINTX_RE ]] || scan "$f" "audit.WriteInTx outside internal/keyscli (use audit.InTx or audit.InPgTx)" '\baudit\.WriteInTx\b'
	fi
	if ! [[ $f =~ $SQL_ALLOW_RE ]] && ! [[ $f == internal/db/queries/audit.sql ]]; then
		scan "$f" "SQL write to an audit table outside internal/audit (add the event to an audit batch)" '\b(insert\s+into|update|delete\s+from|truncate(\s+table)?)\s+(only\s+)?(install_)?audit_(events|chain_heads?)\b'
	fi
done <<<"$files"

if [[ -s $out ]]; then
	echo "FAIL: the audit tables are written only by internal/audit (audit batch, ADR 0100 decision 1.7):" >&2
	sort -u "$out" | head -50 >&2
	exit 1
fi
echo "audit-batch-lint: $n files, no audit write outside internal/audit"
