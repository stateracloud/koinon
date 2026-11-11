#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Host guards of hack/go-test.sh, sourced by it and by hack/go-test-host-test.sh (issue 551).
# A Mac that sleeps in the middle of a run stalls Docker and its port forwards, and every suite that
# shares the run's Postgres times out together; the run then reports dozens of product failures.
# These functions keep the host awake for the run, refuse a start when Docker or its clock is wrong,
# say after the run that the host slept, and cap the heavy runs on one machine.
#
# Prior art: caffeinate(8) `-w <pid>` is the documented way to tie an assertion to a process (no
# timer to run out); `pmset -g log` is Apple's own record of Sleep/Wake events; the slot lock is a
# mkdir lock, the portable form (macOS has no flock) used by git's own lockfiles and by GNU make's
# jobserver idea of "N tokens for the whole machine".
#
# Test hooks (env): KOINON_UNAME, KOINON_PMSET, KOINON_DOCKER, KOINON_CAFFEINATE name the commands
# to use instead of uname, pmset, docker and caffeinate.

host_os() { "${KOINON_UNAME:-uname}" -s; }
host_is_mac() { [[ $(host_os) == Darwin ]]; }

# Keep the machine awake until the process $1 (default: this shell) ends. -i idle sleep, -s system
# sleep (AC power only; on battery macOS ignores it, so say so). Not on macOS: nothing happens.
host_keep_awake() {
	host_is_mac || return 0
	local cmd=${KOINON_CAFFEINATE:-caffeinate}
	command -v "$cmd" >/dev/null 2>&1 || { echo "go-test.sh: no caffeinate; the Mac may sleep during the run" >&2; return 0; }
	"$cmd" -i -s -w "${1:-$$}" >/dev/null 2>&1 &
	# shellcheck disable=SC2034 # read by the caller
	host_caffeinate_pid=$!
	local pm=${KOINON_PMSET:-pmset}
	if command -v "$pm" >/dev/null 2>&1 && "$pm" -g batt 2>/dev/null | grep -q "Battery Power"; then
		echo "go-test.sh: running on battery; caffeinate -s does not stop sleep without AC power. Plug in for a long run" >&2
	fi
	return 0
}

# Seconds since the epoch of an ISO 8601 time as Docker prints it (2026-11-11T11:44:29.123456789Z,
# or with an offset). Prints nothing when it cannot parse.
host_iso_epoch() {
	local t=$1
	t=$(sed -E 's/\.[0-9]+//; s/Z$/+0000/; s/([+-][0-9]{2}):([0-9]{2})$/\1\2/' <<<"$t")
	date -d "$t" +%s 2>/dev/null || date -j -f '%Y-%m-%dT%H:%M:%S%z' "$t" +%s 2>/dev/null || true
}

# Ask Docker for its clock under a deadline of $1 seconds (default 5). Prints the daemon's
# SystemTime; returns 0 answered, 1 no daemon (answered with an error: the suites skip as before),
# 2 no answer within the deadline.
host_docker_time() {
	local limit=${1:-5} docker=${KOINON_DOCKER:-docker} out pid i
	command -v "$docker" >/dev/null 2>&1 || return 1
	out=$(mktemp "${TMPDIR:-/tmp}/go-test-docker.XXXXXX") || return 1
	"$docker" info --format '{{.SystemTime}}' >"$out" 2>/dev/null &
	pid=$!
	for ((i = 0; i < limit * 10; i++)); do
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.1
	done
	if kill -0 "$pid" 2>/dev/null; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
		rm -f "$out"
		return 2
	fi
	if wait "$pid" 2>/dev/null; then
		cat "$out"
		rm -f "$out"
		return 0
	fi
	rm -f "$out"
	return 1
}

# Before the tests: Docker answers, and its clock (the daemon's VM, which every container shares)
# is within KOINON_CLOCK_SKEW seconds (default 2) of the host's. Returns 3 with a message when not.
# No Docker CLI or a daemon that answers with an error is not a problem here: testenv skips.
host_docker_preflight() {
	local ts rc=0 dock now skew max=${KOINON_CLOCK_SKEW:-2}
	ts=$(host_docker_time "${KOINON_DOCKER_PING_SECONDS:-5}") || rc=$?
	case $rc in
	0) ;;
	2)
		echo "go-test.sh: Docker did not answer within ${KOINON_DOCKER_PING_SECONDS:-5} s; restart it (OrbStack: orb restart) and run again" >&2
		return 3
		;;
	*) return 0 ;;
	esac
	dock=$(host_iso_epoch "$ts")
	[[ -n $dock ]] || { echo "go-test.sh: cannot read the Docker clock ($ts); skipping the clock check" >&2; return 0; }
	now=$(date +%s)
	skew=$((dock > now ? dock - now : now - dock))
	if ((skew > max)); then
		echo "go-test.sh: the Docker clock differs from this host by ${skew} s (limit ${max} s); time-based specs would fail. Restart Docker (OrbStack: orb restart) and run again" >&2
		return 3
	fi
	return 0
}

# Did the host sleep between two local times ("YYYY-MM-DD HH:MM:SS")? Reads the Sleep events of
# `pmset -g log`, whose lines start with the local time. Returns 0 when it slept. Not on macOS, or
# with no pmset: 1.
host_slept_between() {
	host_is_mac || return 1
	local pm=${KOINON_PMSET:-pmset}
	command -v "$pm" >/dev/null 2>&1 || return 1
	"$pm" -g log 2>/dev/null | awk -v a="$1" -v b="$2" '
		$3 ~ /^[+-][0-9]{4}$/ && $4 == "Sleep" { t = $1 " " $2; if (t >= a && t <= b) found = 1 }
		END { exit !found }'
}

# Slots: at most KOINON_GO_SLOTS (default 2) heavy runs at once. A slot is a directory holding the
# pid of its owner; a dead owner's slot is taken over. Waits, printing one line. Sets
# host_slot_dir, which host_release_slot removes. Not on a Linux CI runner unless KOINON_GO_SLOTS is
# set: those runs are alone on their machine.
host_acquire_slot() {
	if ! host_is_mac && [[ -z ${KOINON_GO_SLOTS:-} ]]; then return 0; fi
	local n=${KOINON_GO_SLOTS:-2} base=${KOINON_GO_SLOTS_DIR:-${TMPDIR:-/tmp}/koinon-go-slots} i d owner told=0
	mkdir -p "$base"
	while :; do
		for ((i = 1; i <= n; i++)); do
			d=$base/slot.$i
			if mkdir "$d" 2>/dev/null; then
				echo "$$" >"$d/pid"
				host_slot_dir=$d
				return 0
			fi
			owner=$(cat "$d/pid" 2>/dev/null || true)
			if [[ -n $owner ]] && ! kill -0 "$owner" 2>/dev/null; then
				rm -rf "$d"
				continue 2
			fi
		done
		((told)) || { echo "go-test.sh: waiting for a slot ($n heavy runs at most on this machine; KOINON_GO_SLOTS, dir $base)" >&2; told=1; }
		sleep "${KOINON_GO_SLOT_POLL:-2}"
	done
}

host_release_slot() {
	[[ -z ${host_slot_dir:-} ]] || rm -rf "$host_slot_dir"
	host_slot_dir=
}
