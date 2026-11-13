#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The main agent's state files (lanes.md and pending.md), written through one script instead of by
# hand: every write is atomic (temp file in the same dir, then mv) and serialised by a kernel lock on a lock file, so
# two sessions cannot lose each other's lines. Existing content is never deleted.
#
#   agent-state.sh pending add "<text>"        append "- 2026-11-02 ~22:45 <text>" to pending.md
#   agent-state.sh pending done <match>        mark the ONE open item containing <match> done, in place, by
#                                              appending " [done 2026-11-03 ~09:00]" (the leading date stays,
#                                              so hack/notes-expiry.sh still retires it); several matches:
#                                              refuses, lists them numbered, exit 1; a second run changes nothing
#   agent-state.sh pending done --all <match>  mark every open item containing <match>
#   agent-state.sh pending done --line <n>     mark the item on file line <n> (the number show prints)
#   agent-state.sh pending done --before <date>  mark every open item dated before YYYY-MM-DD (bulk retire)
#   agent-state.sh lane add <N> <lane> <branch> <pr> [note]
#   agent-state.sh lane set <N> <field>=<value>...   fields: lane, branch, pr, note
#   agent-state.sh lane done <N>               drop the row from the table (the log keeps it)
#   agent-state.sh show [--all]                open lanes and open pending items of the last 3 days, numbered
#                                              by file line; --all adds the older ones
#
# lanes.md keeps a small current-lanes table between two marker comments at the top; everything
# else (the log: "- <date> ~<time> <text>" lines and any headed blocks) stays below it. Each lane
# command also appends a log line.
#
# State dir: $KOINON_STATE_DIR (default ${XDG_CONFIG_HOME:-~/.config}/koinon/state). Lock wait: $KOINON_STATE_LOCK_TIMEOUT
# seconds (default 60: it must outlast a full queue of writers, each holding the lock for a whole bash start, not just one holder). Exit: 0 ok, 1 error (no such lane, no match, lock timeout), 2 usage.
# Internal, not for callers: --locked-token, KOINON_STATE_LOCKED, KOINON_STATE_LOCKER (force flock or perl),
# KOINON_STATE_TESTONLY_HOLD (a test hook: hold the lock until killed).
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -euo pipefail

state_dir=${KOINON_STATE_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/koinon/state}
lanes="$state_dir/lanes.md"
pending="$state_dir/pending.md"
begin_marker='<!-- agent-state:lanes begin -->'
end_marker='<!-- agent-state:lanes end -->'

die() { echo "agent-state: $*" >&2; exit 1; }
usage() { sed -n '4,25p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
stamp() { date '+%Y-%m-%d ~%H:%M'; }

file_mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

# lock: serialise writers with a kernel lock on a lock FILE (flock where it exists, perl's flock
# otherwise; macOS has no flock(1)). The script re-runs itself under the lock with
# KOINON_STATE_LOCKED=1, so the whole read-modify-rename is covered, and the kernel releases the
# lock when the holder dies, however it dies. KOINON_STATE_LOCKER=flock|perl forces one path (tests).
# The child gets the token twice, as --locked-token <t> on argv and as KOINON_STATE_LOCKED=<t> in
# the environment; it skips the lock only when both match, so a stale inherited variable cannot.
locked_token=''
if [[ ${1:-} == --locked-token ]]; then
	[[ -n ${2:-} && ${KOINON_STATE_LOCKED:-} == "$2" ]] && locked_token=$2
	shift 2
fi
orig_args=("$@")
# shellcheck disable=SC2016  # perl code, no shell expansion wanted
perl_locker='use Fcntl qw(:flock F_SETFD);
my ($f, $t, @cmd) = @ARGV;
open(my $fh, ">>", $f) or do { print STDERR "agent-state: cannot open $f: $!\n"; exit 1 };
$SIG{ALRM} = sub { exit 75 };
alarm($t);
flock($fh, LOCK_EX) or do { print STDERR "agent-state: flock $f: $!\n"; exit 1 };
alarm(0);
fcntl($fh, F_SETFD, 0);
exec @cmd or do { print STDERR "agent-state: exec failed: $!\n"; exit 1 };'
lock() {
	if [[ -n $locked_token ]]; then
		if [[ -n ${KOINON_STATE_TESTONLY_HOLD:-} ]]; then # tests: hold the lock until killed
			echo "$BASHPID" >"$KOINON_STATE_TESTONLY_HOLD.pid"
			read -r -t 60 <>"$KOINON_STATE_TESTONLY_HOLD" || true
		fi
		return 0
	fi
	mkdir -p "$state_dir"
	local token="$$-$RANDOM$RANDOM-$SECONDS" t=${KOINON_STATE_LOCK_TIMEOUT:-60} lf="$state_dir/.lock" locker=${KOINON_STATE_LOCKER:-} rc=0
	if [[ -z $locker ]]; then if command -v flock >/dev/null 2>&1; then locker=flock; else locker=perl; fi; fi
	case $locker in
	flock) flock -w "$t" -E 75 "$lf" env "KOINON_STATE_LOCKED=$token" "$BASH" "$0" --locked-token "$token" "${orig_args[@]}" || rc=$? ;;
	perl) perl -e "$perl_locker" "$lf" "$t" env "KOINON_STATE_LOCKED=$token" "$BASH" "$0" --locked-token "$token" "${orig_args[@]}" || rc=$? ;;
	*) die "KOINON_STATE_LOCKER must be flock or perl" ;;
	esac
	((rc != 75)) || die "could not take $lf within ${t}s (another agent-state.sh holds it)"
	exit "$rc"
}

# atomic_write <file>: stdin becomes the file, via a temp file in the same dir. A symlink is written
# through to its target; an existing file keeps its mode (new files are 600, from mktemp).
atomic_write() {
	local target=$1 tmp l
	while [[ -L $target ]]; do
		l=$(readlink "$target")
		[[ $l == /* ]] || l=$(dirname "$target")/$l
		target=$l
	done
	tmp=$(mktemp "$(dirname "$target")/.tmp.XXXXXX")
	if cat >"$tmp"; then
		[[ -e $target ]] && chmod "$(file_mode "$target")" "$tmp"
		mv "$tmp" "$target"
	else
		rm -f "$tmp"
		die "write of $1 failed"
	fi
}

# append_line <file> <line>: the file plus one line, atomically; a missing final newline is added.
append_line() {
	local f=$1 line=$2
	{
		if [[ -s $f ]]; then
			cat "$f"
			[[ $(tail -c1 "$f" | od -An -c | tr -d ' ') == '\n' ]] || printf '\n'
		fi
		printf '%s\n' "$line"
	} | atomic_write "$f"
}

trim() { local s=$1; s=${s#"${s%%[![:space:]]*}"}; s=${s%"${s##*[![:space:]]}"}; printf '%s' "$s"; }
clean() { local s=${1//$'\n'/ }; printf '%s' "${s//|//}"; }

# --- lanes.md: a table between markers, then the untouched rest -------------------------------

rows=()
body=()
read_lanes() {
	rows=()
	body=()
	[[ -f $lanes ]] || return 0
	local in=0 skipblank=0 line
	while IFS= read -r line || [[ -n $line ]]; do
		if ((in)); then
			if [[ $line == "$end_marker" ]]; then in=0; skipblank=1; continue; fi
			[[ $line == '| #'* ]] && rows+=("$line")
			continue
		fi
		if [[ $line == "$begin_marker" ]]; then in=1; continue; fi
		if ((skipblank)); then
			skipblank=0
			[[ -z $line ]] && continue
		fi
		body+=("$line")
	done <"$lanes"
	return 0
}

r_n='' r_lane='' r_branch='' r_pr='' r_note=''
parse_row() {
	local f
	IFS='|' read -ra f <<<"$1"
	r_n=$(trim "${f[1]:-}") r_lane=$(trim "${f[2]:-}") r_branch=$(trim "${f[3]:-}")
	r_pr=$(trim "${f[4]:-}") r_note=$(trim "${f[5]:-}")
}
format_row() { printf '| %s | %s | %s | %s | %s |' "$r_n" "$r_lane" "$r_branch" "$r_pr" "$r_note"; }

# write_lanes <log line>: the table, the body and the log line, in one rename.
write_lanes() {
	{
		printf '%s\n' "$begin_marker"
		printf '%s\n' '## Open lanes (maintained by hack/agent-state.sh lane add|set|done; log below)'
		printf '%s\n' '| Issue | Lane | Branch | PR | Note |' '|---|---|---|---|---|'
		if ((${#rows[@]})); then printf '%s\n' "${rows[@]}"; fi
		printf '%s\n' "$end_marker"
		if ((${#body[@]})); then
			printf '\n'
			printf '%s\n' "${body[@]}"
		else
			printf '\n'
		fi
		printf '%s\n' "$1"
	} | atomic_write "$lanes"
}

# find_row <N>: sets row_idx or fails.
row_idx=-1
find_row() {
	local i
	row_idx=-1
	for i in "${!rows[@]}"; do
		parse_row "${rows[$i]}"
		if [[ $r_n == "#$1" ]]; then row_idx=$i; return 0; fi
	done
	return 1
}

issue_num() {
	local n=${1#\#}
	[[ $n =~ ^[0-9]+$ ]] || { echo "agent-state: issue number expected, got '$1'" >&2; exit 2; }
	printf '%s' "$n"
}

lane_add() {
	(($# >= 4 && $# <= 5)) || usage
	local n
	n=$(issue_num "$1")
	lock
	read_lanes
	find_row "$n" && die "lane #$n is already open (use: lane set $n <field>=<value>)"
	r_n="#$n" r_lane=$(clean "$2") r_branch=$(clean "$3") r_pr=$(clean "$4") r_note=$(clean "${5:-}")
	rows+=("$(format_row)")
	write_lanes "- $(stamp) lane add #$n $r_lane $r_branch $r_pr${r_note:+: $r_note}"
}

lane_set() {
	(($# >= 2)) || usage
	local n
	n=$(issue_num "$1")
	shift
	lock
	read_lanes
	find_row "$n" || die "lane #$n is not open"
	parse_row "${rows[$row_idx]}"
	local kv key val changes=''
	for kv in "$@"; do
		[[ $kv == *=* ]] || { echo "agent-state: expected field=value, got '$kv'" >&2; exit 2; }
		key=${kv%%=*}
		val=$(clean "${kv#*=}")
		case $key in
		lane) r_lane=$val ;;
		branch) r_branch=$val ;;
		pr) r_pr=$val ;;
		note) r_note=$val ;;
		*) echo "agent-state: unknown field '$key' (lane, branch, pr, note)" >&2; exit 2 ;;
		esac
		changes+="${changes:+ }$key=$val"
	done
	rows[row_idx]=$(format_row)
	write_lanes "- $(stamp) lane set #$n $changes"
}

lane_done() {
	(($# == 1)) || usage
	local n
	n=$(issue_num "$1")
	lock
	read_lanes
	find_row "$n" || die "lane #$n is not open"
	unset 'rows[row_idx]'
	rows=("${rows[@]}")
	write_lanes "- $(stamp) lane done #$n"
}

# --- pending.md ------------------------------------------------------------------------------

is_done() { [[ $1 =~ \[done\ [0-9-]+\ ~[0-9:]+\]$ ]]; }

pending_add() {
	(($# == 1)) && [[ -n $1 ]] || usage
	lock
	append_line "$pending" "- $(stamp) $(clean "$1")"
}

# today: the pinned test date (KOINON_STATE_TODAY) or the real one; days_ago <n> as YYYY-MM-DD.
today() { printf '%s' "${KOINON_STATE_TODAY:-$(date +%F)}"; }
days_ago() {
	local t
	t=$(today)
	date -d "$t -$1 days" +%F 2>/dev/null || date -j -v-"$1"d -f %F "$t" +%F
}
# item_date <line>: the leading YYYY-MM-DD of a "- 2026-11-02 ..." line, empty when undated.
item_date() { [[ $1 =~ ^-[[:space:]]+([0-9]{4}-[0-9]{2}-[0-9]{2})([[:space:]]|$) ]] && printf '%s' "${BASH_REMATCH[1]}"; return 0; }

pending_done() {
	local mode=one arg
	case ${1:-} in
	--all) mode=all; shift ;;
	--line) mode=line; shift ;;
	--before) mode=before; shift ;;
	esac
	(($# == 1)) && [[ -n $1 ]] || usage
	arg=$1
	case $mode in
	line) [[ $arg =~ ^[0-9]+$ ]] || usage ;;
	before) [[ $arg =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || usage ;;
	esac
	lock
	[[ -f $pending ]] || die "no pending.md"
	local lines=() line i d now hit=() hit_done=0
	mapfile -t lines <"$pending"
	now=$(stamp)
	for i in "${!lines[@]}"; do
		line=${lines[$i]}
		[[ $line == '- '* ]] || continue
		case $mode in
		one | all) [[ $line == *"$arg"* ]] || continue ;;
		line) ((i + 1 == arg)) || continue ;;
		before)
			d=$(item_date "$line")
			[[ -n $d && $d < $arg ]] || continue
			;;
		esac
		if is_done "$line"; then hit_done=$((hit_done + 1)); else hit+=("$i"); fi
	done
	if ((${#hit[@]} + hit_done == 0)); then die "no pending item matches ($mode '$arg')"; fi
	if ((${#hit[@]} == 0)); then echo "already done: $hit_done item(s)"; return 0; fi
	if [[ $mode == one ]] && ((${#hit[@]} > 1)); then
		echo "agent-state: ${#hit[@]} open items contain '$arg'; use --line <n> or --all:" >&2
		for i in "${hit[@]}"; do printf '  %s: %s\n' "$((i + 1))" "${lines[$i]}" >&2; done
		exit 1
	fi
	for i in "${hit[@]}"; do lines[i]="${lines[$i]} [done $now]"; done
	printf '%s\n' "${lines[@]}" | atomic_write "$pending"
	echo "marked done: ${#hit[@]} item(s)"
}

# --- show ------------------------------------------------------------------------------------

show() {
	local all=0 i n_open=0 older=0 line d cutoff n=0
	[[ ${1:-} == --all ]] && all=1
	cutoff=$(days_ago 3)
	read_lanes
	echo "Open lanes:"
	for i in "${!rows[@]}"; do
		parse_row "${rows[$i]}"
		printf '  %s %s %s PR %s%s\n' "$r_n" "$r_lane" "$r_branch" "$r_pr" "${r_note:+ - $r_note}"
		n_open=$((n_open + 1))
	done
	((n_open)) || echo "  none"
	echo "Open pending (line: item):"
	n_open=0
	if [[ -f $pending ]]; then
		while IFS= read -r line || [[ -n $line ]]; do
			n=$((n + 1))
			[[ $line == '- '* ]] || continue
			! is_done "$line" || continue
			d=$(item_date "$line")
			if ((!all)) && [[ -n $d && $d < $cutoff ]]; then older=$((older + 1)); continue; fi
			printf '  %s: %s\n' "$n" "$line"
			n_open=$((n_open + 1))
		done <"$pending"
	fi
	((n_open)) || echo "  none"
	((older == 0)) || echo "($older older open items: show --all)"
	return 0
}

cmd=${1:-}
shift || true
case $cmd in
pending)
	sub=${1:-}
	shift || true
	case $sub in
	add) pending_add "$@" ;;
	done) pending_done "$@" ;;
	*) usage ;;
	esac
	;;
lane)
	sub=${1:-}
	shift || true
	case $sub in
	add) lane_add "$@" ;;
	set) lane_set "$@" ;;
	done) lane_done "$@" ;;
	*) usage ;;
	esac
	;;
show) show "$@" ;;
*) usage ;;
esac
