#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Audit batch lint (#371, ADR 0100 decision 1.7 and its audit batch amendment): the audit batch
# (audit.InTx, audit.InPgTx) and the named after-the-fact entry point (audit.RecordAfter) are the only
# ways to write an audit event, so the module alone decides the lock order of the audit heads.
#
# The compiler enforces most of this: the sqlc inserts of the audit tables live in
# internal/audit/internal/auditstore, which Go's internal rule makes unreachable outside internal/audit.
# This lint covers what the compiler cannot see. It fails when Go code, tests included, does any of
# this outside internal/audit and internal/db/store:
#   1. writes the audit tables with SQL (INSERT INTO, UPDATE, DELETE FROM, TRUNCATE, COPY, MERGE INTO
#      on audit_events, audit_chain_heads, install_audit_events, install_audit_chain_head, install_audit_anchor_heads,
#      names in any case, bare, schema-qualified or quoted; a TRUNCATE list is checked item by item,
#      with ONLY and a trailing * accepted), in a Go file or in a sqlc query file of internal/db/queries
#      (the store's queries only read the audit tables);
#   2. has a Go string literal that is, or starts with, an audit table name (any case, optionally
#      schema-qualified or quoted, e.g. "public.audit_events"): the SQL of rule 1 built at run time
#      ("INSERT INTO " + t, fmt.Sprintf, pgx.Identifier{"audit_events"} for CopyFrom);
#   3. uses WriteInTx anywhere but internal/keyscli (the one caller whose transaction outlives a
#      function, see the doc comment of audit.WriteInTx), matched as an identifier so an aliased or
#      dot import is caught too.
#
# Exceptions to rules 1 and 2:
#   - internal/db/migrations/audit_test.go and audit_chain_test.go, whole files: they test the
#     tables' own triggers, grants and hash chain with raw SQL (SQL_ALLOW_RE below);
#   - one statement in a _test.go file, marked on the line directly above the line the hit is
#     reported on (the line of the SQL verb, or of the literal) with a comment line of its own:
#         // audit-lint:allow <reason>
#     The reason is required: at least two words and 10 characters. Use it only for a test that proves the database refuses or detects a
#     write the module would never make (a tamper test, a test of the database's own grants). A
#     marker with no reason, a marker outside a _test.go file, a marker that is not on a line of its
#     own and a marker whose next line has no hit all fail the lint, so a marker cannot drift away
#     from its statement.
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

# Code that owns the audit tables: the module itself (with its auditstore) and the store, whose sqlc
# output has the read queries and the tables' models.
export OWNER_RE='^internal/(audit|db/store)/'
# Rule 3: the one allowed user of audit.WriteInTx.
export WRITEINTX_RE='^internal/keyscli/'
# Rules 1 and 2, whole files: the migration tests of the tables' triggers, grants and hash chain.
export SQL_ALLOW_RE='^internal/db/migrations/audit(_chain)?_test\.go$'

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
my $q = qr/\\?"/;                                      # a double quote, escaped inside a Go "string"
my $tbl = qr/(?i:audit_events|audit_chain_heads|install_audit_events|install_audit_chain_head|install_audit_anchor_heads)/;
my $name = qr/(?:$q$tbl$q|\b$tbl\b)/;                  # bare or quoted table name
my $schema = qr/(?:$q\w+$q|\b\w+)\s*\.\s*/;            # optional schema qualifier, bare or quoted
my $item = qr/(?:only\s+)?(?:$schema)?(?:$q\w+$q|\w+)\s*\*?\s*/i;  # one TRUNCATE list item
my $verb = qr/\b(?:insert\s+into|update|delete\s+from|truncate(?:\s+table)?\s+(?:$item,\s*)*|copy|merge\s+into)\s*(?:only\s+)?/i;
my $sqlre = qr/$verb(?:$schema)?$name/i;
# Rule 2: the Go tokens a quote can open or hide in (comments, raw and interpreted strings, runes),
# and the value of a string literal that is a table name, optionally qualified and quoted.
my $tokre = qr{//[^\n]*|/\*.*?\*/|`[^`]*`|"(?:[^"\\\n]|\\.)*"|\x27(?:[^\x27\\\n]|\\.)*\x27}s;
my $litre = qr/\A\s*(?:(?:"\w+"|\w+)\s*\.\s*)?(?:"$tbl"|$tbl\b)/;
my $markre = qr{^[ \t]*//[ \t]*audit-lint:allow\b[ \t]*(.*?)[ \t]*$}m;
my %msg = (
	sql => "SQL write to an audit table outside internal/audit (add the event to an audit batch)",
	lit => "string literal that is or starts with an audit table name outside internal/audit (dynamic SQL; add the event to an audit batch)",
	wit => "audit.WriteInTx outside internal/keyscli (use audit.InTx or audit.InPgTx)",
	mkr => "audit-lint:allow needs a reason of at least two words: // audit-lint:allow <reason>",
	mkt => "audit-lint:allow is for _test.go files only",
	mkp => "audit-lint:allow goes on a comment line of its own, directly above the statement",
	mku => "audit-lint:allow marks no audit write on the next line",
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
	my $go = $f =~ /\.go\z/;
	my $test = $f =~ /_test\.go\z/;
	my $say = sub { my ($l, $k) = @_; print "$f:$l: $msg{$k}\n" };
	# Markers: line of the statement => line of its marker.
	my (%marked, %used, %own);
	if ($go) {
		while ($src =~ /$markre/g) {
			my ($l, $why) = ($line->($-[0]), $1);
			$own{$l} = 1;
			if (!$test) { $say->($l, "mkt") }
			elsif (length($why) < 10 || $why !~ /\S\s+\S/) { $say->($l, "mkr") }
			else { $marked{$l + 1} = $l }
		}
		while ($src =~ /audit-lint:allow/g) {      # a marker after code on its line
			my $l = $line->($-[0]);
			$say->($l, "mkp") unless $own{$l};
		}
	}
	# $markable: a marker above the line excuses the hit (rules 1 and 2, never rule 3).
	my $report = sub {
		my ($o, $k, $markable) = @_;
		my $l = $line->($o);
		if ($markable && exists $marked{$l}) { $used{$l} = 1; return }
		$say->($l, $k);
	};
	my $hit = sub { my ($re, $k, $markable) = @_; while ($src =~ /$re/g) { $report->($-[0], $k, $markable) } };
	if ($go) {
		$hit->(qr/\bWriteInTx\b/, "wit", 0) unless $f =~ $wit;
	}
	if ($f !~ $allow) {
		$hit->($sqlre, "sql", 1);
		if ($go) {                                 # rule 2: walk the Go tokens that can hold a quote
			while ($src =~ /$tokre/g) {
				my ($o, $t) = ($-[0], $&);
				my $v;
				if ($t =~ /\A`(.*)`\z/s) { $v = $1 }
				elsif ($t =~ /\A"(.*)"\z/s) { ($v = $1) =~ s/\\(.)/$1/g }
				$report->($o, "lit", 1) if defined $v && $v =~ $litre;
			}
		}
	}
	for my $l (sort { $a <=> $b } keys %marked) { $say->($marked{$l}, "mku") unless $used{$l} }
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
