#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# shellcheck disable=SC2016 # stub bodies are written with single quotes on purpose
# Offline test of hack/go-test-host.sh (issue 551): caffeinate on a Mac and not elsewhere, the Docker
# ping and clock check, host-slept detection from `pmset -g log`, the slot lock, and the wiring in
# hack/go-test.sh. Stubs stand in for uname, pmset, docker and caffeinate; no Docker is needed.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d "${TMPDIR:-/tmp}/go-test-host-test.XXXXXX")
bg=()
cleanup() {
	for p in ${bg[@]+"${bg[@]}"}; do kill "$p" 2>/dev/null || true; done
	rm -rf "$tmp"
}
trap cleanup EXIT
fail() { echo "FAIL: $*"; exit 1; }

# shellcheck source=hack/go-test-host.sh
source hack/go-test-host.sh
bin=$tmp/bin
mkdir -p "$bin"
stub() { printf '#!/usr/bin/env bash\n%s\n' "$2" >"$bin/$1"; chmod +x "$bin/$1"; }
iso() { date -u -d "$1" '+%Y-%m-%dT%H:%M:%S.123456789Z' 2>/dev/null || date -u -v"$2" '+%Y-%m-%dT%H:%M:%S.123456789Z'; }

# caffeinate: held with -i -s -w <pid> on Darwin, never started on Linux.
stub uname-mac 'echo Darwin'
stub uname-linux 'echo Linux'
stub caffeinate 'echo "$*" >"'"$tmp"'/caff.args"; exec sleep 30'
stub pmset-ac 'case "$1 $2" in "-g batt") echo "Now drawing from AC Power";; esac'
stub pmset-batt 'case "$1 $2" in "-g batt") echo "Now drawing from Battery Power";; esac'
host_caffeinate_pid=
KOINON_UNAME=$bin/uname-linux KOINON_CAFFEINATE=$bin/caffeinate host_keep_awake $$
[[ -z $host_caffeinate_pid && ! -e $tmp/caff.args ]] || fail "caffeinate started on Linux"
KOINON_UNAME=$bin/uname-mac KOINON_CAFFEINATE=$bin/caffeinate KOINON_PMSET=$bin/pmset-ac host_keep_awake 4242
bg+=("$host_caffeinate_pid")
for _ in 1 2 3 4 5 6 7 8 9 10; do [[ -e $tmp/caff.args ]] && break; sleep 0.2; done
[[ -n $host_caffeinate_pid ]] || fail "caffeinate not started on Darwin"
[[ $(<"$tmp/caff.args") == "-i -s -w 4242" ]] || fail "caffeinate args: $(<"$tmp/caff.args")"
KOINON_UNAME=$bin/uname-mac KOINON_CAFFEINATE=$bin/caffeinate KOINON_PMSET=$bin/pmset-batt host_keep_awake $$ 2>"$tmp/batt.err"
bg+=("$host_caffeinate_pid")
grep -q 'battery' "$tmp/batt.err" || fail "no battery warning"
echo "ok: caffeinate -i -s -w <pid> on macOS only, battery warning"

# Docker ping and clock: a fresh clock passes, 10 s of skew and a hang stop the run with 3, an error is no problem.
skew_iso=$(iso "10 seconds ago" -10S)
stub docker-ok 'date -u +%Y-%m-%dT%H:%M:%S.123456789Z'
stub docker-skew "echo $skew_iso"
stub docker-off 'echo "Cannot connect to the Docker daemon" >&2; exit 1'
stub docker-hang 'exec sleep 30'
KOINON_DOCKER=$bin/docker-ok host_docker_preflight || fail "a matching clock must pass"
rc=0
out=$(KOINON_DOCKER=$bin/docker-skew host_docker_preflight 2>&1) || rc=$?
{ [[ $rc == 3 ]] && grep -q 'orb restart' <<<"$out"; } || fail "10 s skew: rc=$rc out=$out"
KOINON_CLOCK_SKEW=60 KOINON_DOCKER=$bin/docker-skew host_docker_preflight || fail "KOINON_CLOCK_SKEW=60 must allow 10 s"
KOINON_DOCKER=$bin/docker-off host_docker_preflight || fail "a daemon that is off is a skip, not a stop"
KOINON_DOCKER=$bin/no-such-docker host_docker_preflight || fail "no docker CLI is a skip, not a stop"
rc=0
out=$(KOINON_DOCKER_PING_SECONDS=1 KOINON_DOCKER=$bin/docker-hang host_docker_preflight 2>&1) || rc=$?
{ [[ $rc == 3 ]] && grep -q 'did not answer' <<<"$out"; } || fail "hung docker: rc=$rc out=$out"
echo "ok: docker preflight passes, stops on skew and on a hang, skips without a daemon"

# An offset time parses to the same instant as the Z time.
[[ $(host_iso_epoch 2026-11-11T12:00:00.5+01:00) == "$(host_iso_epoch 2026-11-11T11:00:00Z)" ]] || fail "offset time parse"
echo "ok: Docker time formats parse"

# Host slept: a Sleep event inside the window counts; one before it, Wake lines and Linux do not.
stub pmset-log 'cat <<LOG
2026-11-11 09:40:00 +0100 Sleep               	Entering Sleep state due to Idle Sleep
2026-11-11 09:46:48 +0100 Wake                	DarkWake to FullWake
2026-11-11 09:47:18 +0100 Sleep               	Entering DarkWake state due to Notification
2026-11-11 12:36:30 +0100 Wake                	DarkWake to FullWake
2026-11-11 12:40:00 +0100 Assertions          	PID 1(caffeinate) Created
LOG'
export KOINON_UNAME=$bin/uname-mac KOINON_PMSET=$bin/pmset-log
host_slept_between "2026-11-11 09:47:00" "2026-11-11 12:50:00" || fail "sleep inside the window not seen"
! host_slept_between "2026-11-11 09:47:19" "2026-11-11 12:50:00" || fail "sleep before the window counted"
! host_slept_between "2026-11-11 12:00:00" "2026-11-11 12:30:00" || fail "a Wake line counted as sleep"
! KOINON_UNAME=$bin/uname-linux host_slept_between "2026-11-11 09:00:00" "2026-11-11 13:00:00" || fail "Linux has no sleep check"
unset KOINON_UNAME KOINON_PMSET
echo "ok: host slept is read from pmset Sleep events inside the run window"

# Slots: two at once, a third waits and gets a slot when an owner ends; release frees it.
export KOINON_GO_SLOTS=2 KOINON_GO_SLOTS_DIR=$tmp/slots KOINON_GO_SLOT_POLL=0.2
mkdir -p "$KOINON_GO_SLOTS_DIR/slot.1" "$KOINON_GO_SLOTS_DIR/slot.2"
exec 9< <(exec sleep 30)
owner=$!
exec 9<&-
bg+=("$owner")
echo "$owner" >"$KOINON_GO_SLOTS_DIR/slot.1/pid"
echo "$owner" >"$KOINON_GO_SLOTS_DIR/slot.2/pid"
(
	host_slot_dir=
	host_acquire_slot 2>"$tmp/wait.err"
	echo got >"$tmp/got"
	host_release_slot
) &
waiter=$!
bg+=("$waiter")
sleep 1
[[ ! -e $tmp/got ]] || fail "a third run took a slot while two were held"
grep -q 'waiting for a slot' "$tmp/wait.err" || fail "no waiting message"
kill "$owner"
wait "$waiter" || fail "waiter failed"
[[ -e $tmp/got ]] || fail "waiter never got a slot after the owners ended"
[[ $(/bin/ls "$KOINON_GO_SLOTS_DIR") == slot.2 ]] || fail "slot not released: $(/bin/ls "$KOINON_GO_SLOTS_DIR")"
unset KOINON_GO_SLOTS KOINON_GO_SLOTS_DIR KOINON_GO_SLOT_POLL
echo "ok: at most N runs hold a slot, a dead owner's slot is reused, release frees it"

# Wiring in go-test.sh.
for pat in 'host_keep_awake' 'host_docker_preflight' 'host_acquire_slot' 'host_slept_between' 'reason="host slept"' '-timeout "\$go_timeout"' '--timeout="\$ginkgo_timeout"' '--procs=4'; do
	grep -q -- "$pat" hack/go-test.sh || fail "go-test.sh does not use $pat"
done
grep -q 'host slept' hack/lane-gates.sh || fail "lane-gates does not classify host slept"
echo "ok: go-test.sh and lane-gates are wired to the host guards"

# End to end with stubs: a Darwin host whose log shows a Sleep makes a run exit 3 with the verdict line.
stub pmset-slept 'case "$1 $2" in "-g batt") ;; *) echo "$(date "+%Y-%m-%d %H:%M:%S") +0000 Sleep Entering Sleep";; esac'
stub caffeinate-quick 'exit 0'
rc=0
out=$(KOINON_UNAME=$bin/uname-mac KOINON_PMSET=$bin/pmset-slept KOINON_CAFFEINATE=$bin/caffeinate-quick KOINON_DOCKER=$bin/docker-off \
	KOINON_GO_SLOTS_DIR=$tmp/slots2 REPORT_DIR=$tmp/reports hack/go-test.sh ./internal/project 2>&1) || rc=$?
[[ $rc == 3 ]] || { echo "$out" | tail -10; fail "a run that slept must exit 3, got $rc"; }
grep -q 'go-test verdict: notrun reason="host slept"' <<<"$out" || { echo "$out" | tail -10; fail "no host slept verdict"; }
echo "ok: a run during which the host slept exits 3 with notrun reason=\"host slept\""
