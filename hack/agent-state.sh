#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The main agent's state files (lanes.md and pending.md), written through one script instead of by
# hand: every write is atomic (temp file in the same dir, then mv) and serialised by a lock dir, so
# two sessions cannot lose each other's lines. Existing content is never deleted.
#
#   agent-state.sh pending add "<text>"        append "- 2026-11-02 ~22:45 <text>" to pending.md
#   agent-state.sh pending done <match>        mark every open item containing <match> done, in place, by
#                                              appending " [done 2026-11-03 ~09:00]" (the leading date stays,
#                                              so hack/notes-expiry.sh still retires it);
#                                              a second run changes nothing
#   agent-state.sh lane add <N> <lane> <branch> <pr> [note]
#   agent-state.sh lane set <N> <field>=<value>...   fields: lane, branch, pr, note
#   agent-state.sh lane done <N>               drop the row from the table (the log keeps it)
#   agent-state.sh show                        open lanes and open pending items, compact
#
# lanes.md keeps a small current-lanes table between two marker comments at the top; everything
# else (the log: "- <date> ~<time> <text>" lines and any headed blocks) stays below it. Each lane
# command also appends a log line.
#
# State dir: $KOINON_STATE_DIR (default ~/.config/koinon/state). Lock wait: $KOINON_STATE_LOCK_TIMEOUT
# seconds (default 10). Exit: 0 ok, 1 error (no such lane, no match, lock timeout), 2 usage.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -euo pipefail

state_dir=${KOINON_STATE_DIR:-$HOME/.config/koinon/state}
lanes="$state_dir/lanes.md"
pending="$state_dir/pending.md"
begin_marker='<!-- agent-state:lanes begin -->'
end_marker='<!-- agent-state:lanes end -->'
lock_dir="$state_dir/.lock"
lock_held=0

die() { echo "agent-state: $*" >&2; exit 1; }
usage() { sed -n '4,19p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
stamp() { date '+%Y-%m-%d ~%H:%M'; }

unlock() { if ((lock_held)); then rmdir "$lock_dir" 2>/dev/null || true; lock_held=0; fi; }
lock() {
	mkdir -p "$state_dir"
	local tries=0 max=$((${KOINON_STATE_LOCK_TIMEOUT:-10} * 20))
	until mkdir "$lock_dir" 2>/dev/null; do
		tries=$((tries + 1))
		((tries <= max)) || die "could not take $lock_dir within ${KOINON_STATE_LOCK_TIMEOUT:-10}s (remove it if no agent-state.sh is running)"
		sleep 0.05
	done
	lock_held=1
	trap unlock EXIT
	trap 'exit 130' INT TERM
}

# atomic_write <file>: stdin becomes the file, via a temp file in the same dir.
atomic_write() {
	local tmp
	tmp=$(mktemp "$state_dir/.tmp.XXXXXX")
	if cat >"$tmp"; then mv "$tmp" "$1"; else rm -f "$tmp"; die "write of $1 failed"; fi
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
		fi
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
	write_lanes
	append_line "$lanes" "- $(stamp) lane add #$n $r_lane $r_branch $r_pr${r_note:+: $r_note}"
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
	write_lanes
	append_line "$lanes" "- $(stamp) lane set #$n $changes"
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
	write_lanes
	append_line "$lanes" "- $(stamp) lane done #$n"
}

# --- pending.md ------------------------------------------------------------------------------

is_done() { [[ $1 =~ \[done\ [0-9-]+\ ~[0-9:]+\]$ ]]; }

pending_add() {
	(($# == 1)) && [[ -n $1 ]] || usage
	lock
	append_line "$pending" "- $(stamp) $(clean "$1")"
}

pending_done() {
	(($# == 1)) && [[ -n $1 ]] || usage
	lock
	[[ -f $pending ]] || die "no match for '$1' (no pending.md)"
	local lines=() out=() line open=0 already=0 now
	mapfile -t lines <"$pending"
	now=$(stamp)
	for line in "${lines[@]}"; do
		if [[ $line == '- '* && $line == *"$1"* ]]; then
			if is_done "$line"; then
				already=$((already + 1))
			else
				open=$((open + 1))
				line="$line [done $now]"
			fi
		fi
		out+=("$line")
	done
	if ((open + already == 0)); then die "no pending item contains '$1'"; fi
	if ((open == 0)); then echo "already done: $already item(s) match '$1'"; return 0; fi
	printf '%s\n' "${out[@]}" | atomic_write "$pending"
	echo "marked done: $open item(s)"
}

# --- show ------------------------------------------------------------------------------------

show() {
	local i n_open=0 line
	read_lanes
	echo "Open lanes:"
	for i in "${!rows[@]}"; do
		parse_row "${rows[$i]}"
		printf '  %s %s %s PR %s%s\n' "$r_n" "$r_lane" "$r_branch" "$r_pr" "${r_note:+ - $r_note}"
		n_open=$((n_open + 1))
	done
	((n_open)) || echo "  none"
	echo "Open pending:"
	n_open=0
	if [[ -f $pending ]]; then
		while IFS= read -r line || [[ -n $line ]]; do
			if [[ $line == '- '* ]] && ! is_done "$line"; then
				printf '  %s\n' "$line"
				n_open=$((n_open + 1))
			fi
		done <"$pending"
	fi
	((n_open)) || echo "  none"
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
show) show ;;
*) usage ;;
esac
