#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The local gate command (koinon#321): runs the CI gates a change touches and blocks until it has a
# verdict. Replaces an agent that polls test logs. Needs only Docker (and kind for the e2e, which is
# not run here); no lab and no external-run status.
#
#   hack/lane-gates.sh [<base>]     base defaults to origin/main
#   hack/lane-gates.sh --start [<base>]
#                                   detach the same run, print the verdict file path and return at once
#   hack/lane-gates.sh --wait <file> [--max N]
#                                   print the verdict file's gate lines and exit with the run's code;
#                                   75 when the run is still going after N seconds (default 600, 0 = look once)
#
# Which gates: the lint, test and consistency gates of hack/preflight.sh (`--list` picks them from
# the diff) plus the heavy CI jobs hack/ci-changes.sh turns on: chartsmoke (make test-chart-smoke),
# harnesses (make test-harnesses) and escape (make test-admission). Neither list is repeated here.
# Gates run in parallel, LANE_GATES_JOBS at a time (default 4); tidy and generate, which rewrite files of
# the tree, run alone at the end. A gate whose inputs are unchanged since its last green run is reported
# pass with reason="reused ..." (KOINON_GATE_REUSE=0 runs everything; CI never reuses). The e2e gate of
# preflight is informational and is left out.
#
# One line per gate on stdout:
#   gate=<name> rc=<n> seconds=<n> verdict=pass|fail|notrun [reason=<why>] log=<path>
# verdict=notrun means this machine cannot run the gate (no Docker, a missing tool, a sandbox
# denial); the reason says which. It is never reported as fail, and never as pass.
# Exit: 1 when any gate failed, 3 when none failed but some did not run (do not read that as
# verified), 0 when every selected gate passed, 2 on a usage error.
#
# "ok" is printed only when every selected gate passed and at least one gate ran. A crash of
# preflight --list or ci-changes.sh, an empty gate list (gitleaks is always selected) and a CI job
# with no local gate are never "ok": the first two exit 1, the last is notrun (exit 3).
#
# --start / --wait (#654): a lane agent that must hand back cannot block on the run, and a background
# task dies with its turn. --start runs `lane-gates.sh <base>` in its own session (setsid), with its
# output in <dir>/verdict, its pid in <dir>/pid and, when it ends, its exit code in <dir>/rc. --wait
# polls that rc file, so it can be called again after 75. A run whose process vanished without an rc
# (killed, host restart) is exit 1 with that said, never "still running" forever.
#
# Test hooks (hack/lane-gates-test.sh): LANE_GATES_PREFLIGHT, LANE_GATES_CHANGES, LANE_GATES_MAKE
# replace hack/preflight.sh, hack/ci-changes.sh and make; LANE_GATES_MISSING_TOOLS="helm go" hides tools.
# Setting any of them prints a warning on stderr.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

self=$(cd "$(dirname "$0")" && pwd)/$(basename "$0") # before any cd: --start re-runs this file

case ${1:-} in
-h | --help) sed -n 4,35p "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
--wait)
	shift
	wfile="" wmax=600
	while [[ $# -gt 0 ]]; do
		case $1 in
		--max) [[ ${2:-} =~ ^[0-9]+$ ]] || { echo "lane-gates: --max needs a number of seconds" >&2; exit 2; }
			wmax=$2; shift 2 ;;
		-*) echo "lane-gates: unknown argument $1 (see --help)" >&2; exit 2 ;;
		*) [[ -z $wfile ]] || { echo "lane-gates: --wait takes one verdict file" >&2; exit 2; }
			wfile=$1; shift ;;
		esac
	done
	[[ -n $wfile ]] || { echo "lane-gates: --wait needs the verdict file that --start printed" >&2; exit 2; }
	[[ -f $wfile ]] || { echo "lane-gates: no verdict file $wfile" >&2; exit 2; }
	wdir=$(dirname "$wfile")
	waited=0
	while [[ ! -f $wdir/rc ]]; do
		wpid=$(cat "$wdir/pid" 2>/dev/null)
		if [[ -z $wpid ]] || ! kill -0 "$wpid" 2>/dev/null; then
			[[ -f $wdir/rc ]] && break # finished between the two checks
			echo "lane-gates FAIL: the run (pid ${wpid:-unknown}) is gone and left no exit code; nothing was verified (log $wfile)" >&2
			exit 1
		fi
		if [[ $waited -ge $wmax ]]; then
			echo "lane-gates: still running after ${wmax}s (pid $wpid); call --wait $wfile again" >&2
			exit 75
		fi
		sleep 2
		waited=$((waited + 2))
	done
	wrc=$(cat "$wdir/rc")
	if [[ $wrc == 0 || $wrc == 1 || $wrc == 3 ]]; then
		grep -E '^(gate=|lane-gates )' "$wfile"
		exit "$wrc"
	fi
	# 2, 126, 127...: the run itself broke (usage error, missing file); never pass that code on as a verdict.
	echo "lane-gates FAIL: the run exited $wrc before it had a verdict; nothing was verified"
	tail -n 5 "$wfile" | sed 's/^/  | /'
	exit 1
	;;
--start)
	shift
	[[ $# -le 1 ]] || { echo "lane-gates: --start takes at most the base ref" >&2; exit 2; }
	sbase=${1:-origin/main}
	cd "$(git rev-parse --show-toplevel)" || exit 2
	git rev-parse --verify --quiet "$sbase^{commit}" >/dev/null || { echo "lane-gates: unknown base $sbase (git fetch origin?)" >&2; exit 2; }
	sdir=$(mktemp -d "${TMPDIR:-/tmp}/lane-gates-run.XXXXXX") || exit 2
	[[ -f $self ]] || { echo "lane-gates: cannot find $self" >&2; exit 2; }
	: >"$sdir/verdict"
	# shellcheck disable=SC2016 # the script is for the child shell: its $1..$3 must not expand here
	# The detached shell writes rc last and atomically, so --wait never reads a half-written code.
	run='echo $$ >"$3/pid"; bash "$1" "$2" >"$3/verdict" 2>&1; echo $? >"$3/rc.tmp"; mv "$3/rc.tmp" "$3/rc"'
	# A new session, so ending the caller's process group does not end the run (macOS has no setsid).
	if command -v setsid >/dev/null 2>&1; then
		setsid bash -c "$run" _ "$self" "$sbase" "$sdir" </dev/null >/dev/null 2>&1 &
	elif command -v perl >/dev/null 2>&1; then
		perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' bash -c "$run" _ "$self" "$sbase" "$sdir" </dev/null >/dev/null 2>&1 &
	else
		echo "lane-gates: --start needs setsid (util-linux) or perl to detach the run" >&2; exit 2
	fi
	disown 2>/dev/null
	# The pid is the detached shell's own ($$ above; setsid may fork). Wait for it so --wait never races it.
	for _ in $(seq 1 50); do [[ -s $sdir/pid ]] && break; sleep 0.1; done
	[[ -s $sdir/pid ]] || { echo "lane-gates: the detached run did not start" >&2; exit 2; }
	echo "$sdir/verdict"
	exit 0
	;;
-*) echo "lane-gates: unknown argument $1 (see --help)" >&2; exit 2 ;;
esac
[[ $# -le 1 ]] || { echo "lane-gates: at most one argument, the base ref" >&2; exit 2; }
base=${1:-origin/main}

cd "$(git rev-parse --show-toplevel)" || exit 2
git rev-parse --verify --quiet "$base^{commit}" >/dev/null || { echo "lane-gates: unknown base $base (git fetch origin?)" >&2; exit 2; }

# shellcheck source=/dev/null
. "$(dirname "$0")/paths-lib.sh" || exit 2
# shellcheck source=/dev/null
. "$(dirname "$0")/gate-reuse-lib.sh" || exit 2
paths_load || exit 2 # the `lane` lines of hack/paths.rules say which local gate stands for a CI job
preflight=${LANE_GATES_PREFLIGHT:-hack/preflight.sh}
changes=${LANE_GATES_CHANGES:-hack/ci-changes.sh}
make_cmd=${LANE_GATES_MAKE:-make}
export KUBECONFIG=/dev/null
for v in LANE_GATES_PREFLIGHT LANE_GATES_CHANGES LANE_GATES_MAKE LANE_GATES_MISSING_TOOLS; do
	if [[ -n ${!v:-} ]]; then
		echo "lane-gates: warning: test hook $v is set; this run is not a real verdict" >&2
		export KOINON_GATE_REUSE=0 # a stubbed run proves nothing: it neither reuses nor records a verdict
	fi
done

logdir=$(mktemp -d "${TMPDIR:-/tmp}/lane-gates.XXXXXX") || exit 2
failed=0 notrun=0 ran=0

# A real failure marker in a log: a gate with one of these failed, whatever else the log says
# (go-test.sh logs "docker is not available" skips right next to real failures). Includes helm
# (`Error:`), chart-check (`FAIL:` / `assertion(s) failed`), and gitleaks (`leaks found:`), so those
# stay fail when the log also mentions EPERM or Docker (#349).
has_failure() { # <log>
	grep -qE -- '--- FAIL|\[FAIL\]|^FAIL|Summarizing [0-9]+ Failure|verdict: FAILED|^ FAIL |Test Files .*failed|error TS[0-9]+|^ *[0-9]+:[0-9]+ +error |\bSC[0-9]{4}\b|^[^ :]+\.go:[0-9]+:[0-9]+: |^Error:|assertion\(s\) failed|leaks found:' "$1"
}

# Output that says the machine, not the change, was the problem. Failure markers win. Pass
# <ignore-failures> as the second argument when the exit code already says "missing requirement".
env_problem() { # <log> [<ignore-failures>]: prints the reason when the log shows one
	local log=$1
	if [[ -z ${2:-} ]] && has_failure "$log"; then
		return
	fi
	if grep -qF 'reason="host slept"' "$log"; then
		echo "host slept"
	elif grep -qE 'Docker did not answer within|Docker answers but gave no clock|the Docker clock differs from this host' "$log"; then
		echo "docker unhealthy"
	elif grep -qiE 'cannot connect to the docker daemon|docker: command not found|docker\.sock.*(permission denied|no such file)|permission denied while trying to connect to the docker|is the docker daemon running|docker is not available' "$log"; then
		echo "docker unavailable"
	elif grep -qiE 'CONNECT tunnel failed, response 403|proxyconnect tcp|dial tcp: lookup [^ ]+: (no such host|server misbehaving)|not in this command.s allowed_domains|server hosted at that remote is unavailable|the server hosted at that remote' "$log"; then
		echo "network denied"
	elif grep -qE 'deny\([0-9]+\)|Sandbox: .* deny' "$log"; then
		echo "sandbox denial"
	elif grep -qiE 'operation not permitted' "$log"; then
		echo "sandbox denial"
	elif grep -qE 'INCOMPLETE \(missing' "$log"; then
		sed -n 's/.*INCOMPLETE (missing \([^)]*\)).*/missing \1/p' "$log" | tail -1
	fi
}

# have_tool <tool>: docker means a reachable daemon. LANE_GATES_MISSING_TOOLS (test hook) hides tools.
have_tool() {
	case " ${LANE_GATES_MISSING_TOOLS:-} " in *" $1 "*) return 1 ;; esac
	if [[ $1 == docker ]]; then docker info >/dev/null 2>&1; else command -v "$1" >/dev/null 2>&1; fi
}

count_verdict() { # <verdict>
	ran=$((ran + 1))
	case $1 in fail) failed=1 ;; notrun) notrun=1 ;; esac
}

# A gate worker (the gates run in parallel, #990) sets REPORT_TO: its line goes to that file and the
# main shell prints and counts it in plan order. Anything else prints at once.
report() { # <gate> <rc> <seconds> <verdict> <reason> <log>
	local line="gate=$1 rc=$2 seconds=$3 verdict=$4"
	[[ -n $5 ]] && line="$line reason=\"$5\""
	if [[ -n ${REPORT_TO:-} ]]; then
		echo "$line log=$6" >"$REPORT_TO"
		return
	fi
	echo "$line log=$6"
	count_verdict "$4"
}

# run_cmd <gate> <needs> <command...>: <needs> is a space-separated tool list (docker helm go) or "-".
# A missing tool is notrun with its name; otherwise runs the command and classifies it.
run_cmd() {
	local g=$1 needs=$2 log start rc reason
	shift 2
	log=$logdir/$g.log
	local t
	for t in $needs; do
		[[ $t == - ]] && continue
		if ! have_tool "$t"; then
			if [[ $t == docker ]]; then reason="docker unavailable"; else reason="$t not installed"; fi
			report "$g" 0 0 notrun "$reason" "$log"
			return
		fi
	done
	local fp=""
	if gate_reuse_enabled; then
		fp=$(gate_fingerprint job "$g" "$(git merge-base "$base" HEAD)")
		if gate_reusable "lane.$g" "$fp"; then
			report "$g" 0 0 pass "reused: inputs unchanged since the last green run" "$log"
			return
		fi
	fi
	start=$SECONDS
	"$@" >"$log" 2>&1
	rc=$?
	if [[ $rc -eq 0 ]]; then
		[[ -n $fp ]] && gate_record "lane.$g" "$fp"
		report "$g" 0 $((SECONDS - start)) pass "" "$log"
		return
	fi
	reason=$(env_problem "$log")
	if [[ -n $reason ]]; then
		report "$g" "$rc" $((SECONDS - start)) notrun "$reason" "$log"
	else
		report "$g" "$rc" $((SECONDS - start)) fail "" "$log"
	fi
}

# 1. The preflight gates the diff selects, plus the gate of every CI job ci-changes.sh turns on
#    (ci-changes is wider than the preflight selection: go runs for a migration or pricing change).
#    One preflight call per gate, so each has a time and a verdict of its own. A
#    "skip (... not installed)" line is a gate that did not run.
# No committed change (a lane with no commits yet, or only uncommitted edits): ci-changes.sh would
# read the empty list as "everything" and start the 35-minute heavy gates, so do not ask it. The
# path gates are notrun with that reason; the preflight gates below still run.
changes_out="" changes_rc=0
if [[ -z $(git diff --name-only --no-renames "$(git merge-base "$base" HEAD)" HEAD) ]]; then
	report paths 0 0 notrun "no committed change" "$logdir/ci-changes.err"
else
	changes_out=$("$changes" "$base" 2>"$logdir/ci-changes.err")
	changes_rc=$?
fi
if [[ $changes_rc -ne 0 ]]; then
	echo "lane-gates FAIL: $changes failed (exit $changes_rc), so the heavy gates cannot be chosen; nothing was verified" >&2
	sed 's/^/  | /' "$logdir/ci-changes.err" | head -5 >&2
	exit 1
fi
jobs=$(sed -n 's/^\(.*\)=true$/\1/p' <<<"$changes_out")
jobgates=""
for j in $jobs; do
	[[ $(paths_raw lane "$j") == "- preflight" ]] && jobgates="$jobgates $j"
done
list_out=$("$preflight" --base "$base" --list 2>"$logdir/list.err")
list_rc=$?
if [[ $list_rc -ne 0 ]]; then
	echo "lane-gates FAIL: $preflight --list failed (exit $list_rc), so the gates cannot be chosen; nothing was verified" >&2
	sed 's/^/  | /' "$logdir/list.err" | head -5 >&2
	exit 1
fi
pre_gates=$(sed -n 's/^\([a-z0-9]*\):.*/\1/p' <<<"$list_out" | grep -vx e2e)
if [[ -z $pre_gates ]]; then
	echo "lane-gates FAIL: $preflight --list selected no gate (gitleaks is always selected); nothing was verified" >&2
	exit 1
fi
gates=$( { echo "$pre_gates"; for j in $jobgates; do echo "$j"; done; } | awk '!seen[$0]++')
run_pre_gate() { # <gate>: one preflight call, one line
	local g=$1 log start rc secs reason inner
	log=$logdir/$g.log
	start=$SECONDS
	"$preflight" --base "$base" --only "$g" >"$log" 2>&1
	rc=$?
	secs=$((SECONDS - start))
	reason=$(sed -n 's/^'"$g"' skip (\(.*\))$/\1/p' "$log" | head -1)
	# The wrapper log holds preflight's stdout: a `<g> FAIL|INCOMPLETE Ns log=<inner>` line and a
	# 5-line tail. Classify from the full inner log it names, and report that path.
	inner=$(awk -v g="$g" '$1 == g && ($2 == "FAIL" || $2 == "INCOMPLETE") { for (i = 3; i <= NF; i++) if ($i ~ /^log=/) { print substr($i, 5); exit } }' "$log")
	[[ -n $inner && -r $inner ]] && log=$inner
	case $rc in
	0) if [[ -n $reason ]]; then
			report "$g" 0 "$secs" notrun "$reason" "$log"
		elif grep -q "^$g ok .*(reused" "$log"; then
			report "$g" 0 "$secs" pass "reused: inputs unchanged since the last green run" "$log"
		else
			report "$g" 0 "$secs" pass "" "$log"
		fi ;;
	3) # exit 3 is the missing-requirement code of go-test.sh and preflight: notrun by exit code
		reason=$(env_problem "$log" ignore)
		report "$g" 3 "$secs" notrun "${reason:-missing requirement}" "$log" ;;
	2)
		# preflight exits 2 when the pid file of this gate is held (a second run of the same gate, or a
		# stuck one). Say so on the gate line.
		if grep -q 'already in progress' "$log"; then
			report "$g" 2 "$secs" fail "preflight already running this gate in this worktree" "$log"
		else
			reason=$(env_problem "$log")
			if [[ -n $reason ]]; then report "$g" "$rc" "$secs" notrun "$reason" "$log"; else report "$g" "$rc" "$secs" fail "" "$log"; fi
		fi
		;;
	*)
		reason=$(env_problem "$log")
		if [[ -n $reason ]]; then report "$g" "$rc" "$secs" notrun "$reason" "$log"; else report "$g" "$rc" "$secs" fail "" "$log"; fi
		;;
	esac
}

run_job() { # <job>: the heavy CI job's local gate, from its `lane` line
	local j=$1 lane needs target
	lane=$(paths_raw lane "$j")
	case $lane in
	"") report "$j" 0 0 notrun "no local gate for CI job $j" "$logdir/$j.log" ;;
	*)
		needs=${lane%% *} target=${lane#* }
		run_cmd "$j" "${needs//,/ }" "$make_cmd" "$target"
		;;
	esac
}

# 2. Run them in parallel (#990): LANE_GATES_JOBS at a time (default 4), each in its own subshell,
#    its line printed in plan order as soon as it and every earlier one has finished. Two gates change
#    the tree while they run (tidy rewrites go.mod and go.sum and puts them back, generate rewrites the
#    generated code): they run alone, one after the other, when the others are done. Every other gate
#    reads the tree. One preflight call per gate, and preflight keeps one pid file per gate, so the
#    calls do not block each other.
exclusive=" tidy generate "
plan=() # the pool gates and heavy jobs first, the long ones leading; then the exclusive ones
for j in $jobs; do
	[[ $(paths_raw lane "$j") == "- preflight" ]] || plan+=("job:$j")
done
for g in $gates; do
	case "$exclusive" in *" $g "*) ;; *) plan+=("gate:$g") ;; esac
done
excl=()
for g in $gates; do
	case "$exclusive" in *" $g "*) excl+=("gate:$g") ;; esac
done
jobs_max=${LANE_GATES_JOBS:-4}
[[ $jobs_max =~ ^[1-9][0-9]*$ ]] || { echo "lane-gates: LANE_GATES_JOBS must be a positive number" >&2; exit 2; }
printed=0
all=("${plan[@]}" "${excl[@]}")
flush_lines() { # print, in plan order, the lines of the finished gates
	local item name
	while ((printed < ${#all[@]})); do
		item=${all[$printed]}
		name=${item#*:}
		[[ -f $logdir/line.$name ]] || break
		cat "$logdir/line.$name"
		count_verdict "$(sed -n 's/.* verdict=\([a-z]*\).*/\1/p' "$logdir/line.$name")"
		printed=$((printed + 1))
	done
}
run_item() { # <kind:name>: runs in a subshell; always leaves line.<name>
	local item=$1 name=${1#*:}
	REPORT_TO=$logdir/line.$name.tmp
	if [[ ${item%%:*} == job ]]; then run_job "$name"; else run_pre_gate "$name"; fi
	[[ -f $REPORT_TO ]] || echo "gate=$name rc=1 seconds=0 verdict=fail reason=\"the gate worker left no verdict\" log=$logdir/$name.log" >"$REPORT_TO"
	mv "$REPORT_TO" "$logdir/line.$name"
}
wait_slot() { # wait until fewer than $1 workers run
	while (($(jobs -rp | wc -l) >= $1)); do
		wait -n 2>/dev/null || true
		flush_lines
	done
}
for item in "${plan[@]}"; do
	wait_slot "$jobs_max"
	run_item "$item" &
done
wait_slot 1
wait
flush_lines
for item in "${excl[@]}"; do
	(run_item "$item")
	flush_lines
done
flush_lines

if [[ $ran == 0 ]]; then
	echo "lane-gates FAIL: no gate ran; nothing was verified" >&2
	exit 1
fi
if [[ $failed == 1 ]]; then
	echo "lane-gates FAIL (logs in $logdir)"
	exit 1
fi
if [[ $notrun == 1 ]]; then
	echo "lane-gates INCOMPLETE: some gates did not run on this machine (logs in $logdir)"
	exit 3
fi
echo "lane-gates ok (logs in $logdir)"
