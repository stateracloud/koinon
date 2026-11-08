#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Audit batch lint (#371, ADR 0100 decision 1.7 and its audit batch amendment): the audit batch
# (audit.InTx, audit.InPgTx) and the named after-the-fact entry point (audit.RecordAfter) are the only
# ways to write an audit event, so the module alone decides the lock order of the audit heads. Fails
# when Go code, tests included, does any of this outside internal/audit:
#   1. uses audit.Record, audit.RecordErr, audit.RecordInstall or audit.RecordInstallErr (the direct
#      writes step 3 deleted; this keeps them from coming back under the same name). The three
#      distinct names are matched as identifiers, so an aliased or dot import is caught too; Record
#      is matched after "audit." and after the alias of any import of internal/audit, and bare in a
#      file that dot-imports it;
#   2. uses the sqlc insert queries of the audit tables (InsertAuditEvent, InsertInstallAuditEvent);
#   3. writes the audit tables with SQL (INSERT INTO, UPDATE, DELETE FROM, TRUNCATE, COPY, MERGE INTO
#      on audit_events, audit_chain_heads, install_audit_events, install_audit_chain_head, bare,
#      schema-qualified or quoted), or names one in a pgx.Identifier literal (CopyFrom), in a Go file
#      or in a sqlc query file other than internal/db/queries/audit.sql;
#   4. uses WriteInTx anywhere but internal/keyscli (the one caller whose transaction outlives a
#      function, see the doc comment of audit.WriteInTx), matched as an identifier so an aliased or
#      dot import is caught too.
# A test that must break the tables on purpose (a tamper test, a test of the database's own grants) is
# allowed by path, listed in SQL_ALLOW below with the reason. Add a path there only for a test that
# proves the database refuses or detects a write the module would never make.
#
# In a git checkout it scans the tracked files and the untracked ones git does not ignore (a new file
# is checked before it is added); a tracked file deleted from disk is skipped. With a root that is not
# a git checkout it scans every file. One perl process scans the whole list.
#
# Usage: hack/audit-batch-lint.sh [root]    (default: the repository root)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=${1:-$(cd "$(dirname "$0")/.." && pwd)}
cd "$root"

# Code that owns the audit tables: the module itself and the sqlc output of its queries.
export OWNER_RE='^internal/(audit|db/store)/'
# Rule 4: the one allowed user of audit.WriteInTx.
export WRITEINTX_RE='^internal/keyscli/'
# Rule 3: the sqlc query file of the module.
export AUDIT_SQL='internal/db/queries/audit.sql'
# Rule 3 allowlist, by path. Each of these tests writes an audit table directly on purpose.
export SQL_ALLOW_RE='^(internal/db/migrations/audit(_chain)?_test\.go|internal/api/audit_test\.go|internal/cmd/koinonapi/audit_test\.go|test/chartsmoke/smoke_test\.go)$'
#   internal/db/migrations/audit*_test.go test the tables' own triggers, grants and hash chain with raw SQL
#   internal/api/audit_test.go            inserts an install event as the database role under test, to show the
#                                         database accepts the writer role and refuses another
#   internal/cmd/koinonapi/audit_test.go tampers with a row (session_replication_role = replica) so
#                                         "audit verify" has a break to report
#   test/chartsmoke/smoke_test.go         inserts through the chart's install audit writer role, to show the
#                                         rendered grants work

list() {
	if git rev-parse --is-inside-work-tree >/dev/null 2>&1 && [[ $(git rev-parse --show-toplevel) == "$(pwd -P)" ]]; then
		git ls-files -z --cached --others --exclude-standard -- '*.go' 'internal/db/queries/*.sql'
	else
		find . \( -name '*.go' -o -path '*/internal/db/queries/*.sql' \) -type f -print0
	fi
}

out=$(mktemp "${TMPDIR:-/tmp}/audit-batch-lint.XXXXXX") || exit 1
trap 'rm -f "$out"' EXIT

# Reads NUL-separated paths on stdin, prints "path:line: message" per hit to stdout and the number of
# files scanned to stderr (an error message instead, with a non-zero exit, when a file cannot be read).
# shellcheck disable=SC2016 # perl source, not shell
prog='
use strict; use warnings;
my ($owner, $wit, $allow) = map { qr/$_/ } @ENV{qw(OWNER_RE WRITEINTX_RE SQL_ALLOW_RE)};
my $auditsql = $ENV{AUDIT_SQL};
my $q = qr/\\?"/;                                      # a double quote, escaped inside a Go "string"
my $tbl = qr/(?:install_)?audit_(?:events|chain_heads?)/;
my $name = qr/(?:$q$tbl$q|\b$tbl\b)/;                  # bare or quoted table name
my $schema = qr/(?:$q\w+$q|\b\w+)\s*\.\s*/;            # optional schema qualifier, bare or quoted
my $verb = qr/\b(?:insert\s+into|update|delete\s+from|truncate(?:\s+table)?|copy|merge\s+into)\s+(?:only\s+)?/i;
my $sqlre = qr/$verb(?:$schema)?$name/i;
my $identre = qr/\bIdentifier\s*\{[^{}]*$q$tbl$q/;
my $importre = qr/^\s*(?:import\s+)?(\.|\w+)?\s*"[^"\n]*\/internal\/audit"/m;
my %msg = (
	rec => "direct audit write (use audit.InTx/InPgTx, or audit.RecordAfter for an after-the-fact event)",
	ins => "store insert of an audit table outside internal/audit (add the event to an audit batch)",
	wit => "audit.WriteInTx outside internal/keyscli (use audit.InTx or audit.InPgTx)",
	sql => "SQL write to an audit table outside internal/audit (add the event to an audit batch)",
);
my $n = 0;
local $/ = "\0";
while (my $f = <STDIN>) {
	chomp $f; $f =~ s{^\./}{};
	next unless length $f && -f $f;
	$n++;
	next if $f =~ $owner;
	open(my $fh, "<", $f) or die "$f: $!\n";
	my $src = do { local $/; <$fh> };
	close $fh;
	my @nl;                                            # offsets of newlines, for line numbers
	while ($src =~ /\n/g) { push @nl, $-[0] }
	my $line = sub { my $o = shift; my ($lo, $hi) = (0, scalar @nl); while ($lo < $hi) { my $m = ($lo + $hi) >> 1; if ($nl[$m] < $o) { $lo = $m + 1 } else { $hi = $m } } $lo + 1 };
	my $hit = sub { my ($re, $k) = @_; while ($src =~ /$re/g) { print "$f:", $line->($-[0]), ": $msg{$k}\n" } };
	my $go = $f =~ /\.go\z/;
	if ($go) {
		my %alias = (audit => 1); my $dot = 0;
		while ($src =~ /$importre/g) { my $as = $1 // "audit"; if ($as eq ".") { $dot = 1 } elsif ($as ne "_") { $alias{$as} = 1 } }
		my $al = join "|", map { quotemeta } sort keys %alias;
		$hit->(qr/\bRecord(?:Err|Install|InstallErr)\b/, "rec");
		$hit->(qr/\b(?:$al)\.Record\b/, "rec");
		$hit->(qr/(?<![.\w])Record\s*\(/, "rec") if $dot;
		$hit->(qr/\b(?:InsertAuditEvent|InsertInstallAuditEvent)(?:Params)?\b/, "ins");
		$hit->(qr/\bWriteInTx\b/, "wit") unless $f =~ $wit;
	}
	if ($f !~ $allow && $f ne $auditsql) {
		$hit->($sqlre, "sql");
		$hit->($identre, "sql") if $go;
	}
}
print STDERR "$n\n";
'

n=$(list | perl -e "$prog" 2>&1 >"$out") || { printf '%s\n' "$n" >&2; exit 1; }

if [[ -s $out ]]; then
	echo "FAIL: the audit tables are written only by internal/audit (audit batch, ADR 0100 decision 1.7):" >&2
	sort -u "$out" | head -50 >&2
	exit 1
fi
echo "audit-batch-lint: $n files, no audit write outside internal/audit"
