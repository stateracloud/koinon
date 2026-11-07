#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The local gate command (koinon#321): runs the CI gates a change touches and blocks until it has a
# verdict. Replaces an agent that polls test logs. Needs only Docker (and kind for the e2e, which is
# not run here); no lab and no external-run status.
#
#   hack/lane-gates.sh [<base>]     base defaults to origin/main
#
# Which gates: the lint, test and consistency gates of hack/preflight.sh (`--list` picks them from
# the diff) plus the heavy CI jobs hack/ci-changes.sh turns on: chartsmoke (make test-chart-smoke),
# flavors (make test-flavors) and escape (make test-admission). Neither list is repeated here.
# Gates run one after the other; the e2e gate of preflight is informational and is left out.
#
# One line per gate on stdout:
#   gate=<name> rc=<n> seconds=<n> verdict=pass|fail|notrun [reason=<why>] log=<path>
# verdict=notrun means this machine cannot run the gate (no Docker, a missing tool, a sandbox
# denial); the reason says which. It is never reported as fail, and never as pass.
# Exit: 1 when any gate failed, 3 when none failed but some did not run (do not read that as
# verified), 0 when every selected gate passed, 2 on a usage error.
#
# Test hooks (hack/lane-gates-test.sh): LANE_GATES_PREFLIGHT, LANE_GATES_CHANGES, LANE_GATES_MAKE
# replace hack/preflight.sh, hack/ci-changes.sh and make.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

case ${1:-} in
-h | --help) sed -n 4,22p "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
-*) echo "lane-gates: unknown argument $1 (see --help)" >&2; exit 2 ;;
esac
[[ $# -le 1 ]] || { echo "lane-gates: at most one argument, the base ref" >&2; exit 2; }
base=${1:-origin/main}

cd "$(git rev-parse --show-toplevel)" || exit 2
git rev-parse --verify --quiet "$base^{commit}" >/dev/null || { echo "lane-gates: unknown base $base (git fetch origin?)" >&2; exit 2; }

preflight=${LANE_GATES_PREFLIGHT:-hack/preflight.sh}
changes=${LANE_GATES_CHANGES:-hack/ci-changes.sh}
make_cmd=${LANE_GATES_MAKE:-make}
export KUBECONFIG=/dev/null

logdir=$(mktemp -d "${TMPDIR:-/tmp}/lane-gates.XXXXXX") || exit 2
failed=0 notrun=0

# Output that says the machine, not the change, was the problem.
env_problem() { # <log>: prints the reason when the log shows one
	local log=$1
	if grep -qiE 'cannot connect to the docker daemon|docker: command not found|docker\.sock.*(permission denied|no such file)|is the docker daemon running' "$log"; then
		echo "docker unavailable"
	elif grep -qiE 'operation not permitted|sandbox' "$log"; then
		echo "sandbox denial"
	elif grep -qE 'INCOMPLETE \(missing' "$log"; then
		sed -n 's/.*INCOMPLETE (missing \([^)]*\)).*/missing \1/p' "$log" | tail -1
	fi
}

report() { # <gate> <rc> <seconds> <verdict> <reason> <log>
	local line="gate=$1 rc=$2 seconds=$3 verdict=$4"
	[[ -n $5 ]] && line="$line reason=\"$5\""
	echo "$line log=$6"
	case $4 in fail) failed=1 ;; notrun) notrun=1 ;; esac
}

# run_cmd <gate> <needs> <command...>: <needs> is "docker" or "-". Runs the command, classifies it.
run_cmd() {
	local g=$1 needs=$2 log start rc reason
	shift 2
	log=$logdir/$g.log
	if [[ $needs == docker ]] && ! docker info >/dev/null 2>&1; then
		report "$g" 0 0 notrun "docker unavailable" "$log"
		return
	fi
	start=$SECONDS
	"$@" >"$log" 2>&1
	rc=$?
	if [[ $rc -eq 0 ]]; then
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

# 1. The preflight gates the diff selects. One preflight call per gate, so each has a time and a
#    verdict of its own. A "skip (... not installed)" line is a gate that did not run.
gates=$("$preflight" --base "$base" --list 2>/dev/null | sed -n 's/^\([a-z0-9]*\):.*/\1/p' | grep -vx e2e || true)
for g in $gates; do
	log=$logdir/$g.log
	start=$SECONDS
	"$preflight" --base "$base" --only "$g" >"$log" 2>&1
	rc=$?
	secs=$((SECONDS - start))
	reason=$(sed -n 's/^'"$g"' skip (\(.*\))$/\1/p' "$log" | head -1)
	case $rc in
	0) if [[ -n $reason ]]; then report "$g" 0 "$secs" notrun "$reason" "$log"; else report "$g" 0 "$secs" pass "" "$log"; fi ;;
	3) report "$g" 3 "$secs" notrun "$(env_problem "$log")" "$log" ;;
	*)
		reason=$(env_problem "$log")
		if [[ -n $reason ]]; then report "$g" "$rc" "$secs" notrun "$reason" "$log"; else report "$g" "$rc" "$secs" fail "" "$log"; fi
		;;
	esac
done

# 2. The heavy CI jobs the preflight does not run, picked by ci-changes.sh.
jobs=$("$changes" "$base" 2>/dev/null | sed -n 's/^\(.*\)=true$/\1/p')
for j in $jobs; do
	case $j in
	chartsmoke) run_cmd chartsmoke docker "$make_cmd" test-chart-smoke ;;
	flavors) run_cmd flavors docker "$make_cmd" test-flavors ;;
	escape) run_cmd escape - "$make_cmd" test-admission ;;
	esac
done

if [[ $failed == 1 ]]; then
	echo "lane-gates FAIL (logs in $logdir)"
	exit 1
fi
if [[ $notrun == 1 ]]; then
	echo "lane-gates INCOMPLETE: some gates did not run on this machine (logs in $logdir)"
	exit 3
fi
echo "lane-gates ok (logs in $logdir)"
