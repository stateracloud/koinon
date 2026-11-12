# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=sh
#
# sweep ends every process in the sandbox that runs as this script's user, apart from PID 1,
# the sandbox's main process and this script with its ancestors (the exec that runs it).
# koinon-agentd runs it before it starts a harness and after a run's harness has ended, so
# nothing the agent started in one run (a harness's daemon, a background job) lives into the
# next (sweep.go).
#
# POSIX sh, /proc, and grep and sleep from the harness image (every image has them): grep runs
# once before the scan, sleep only between rounds, and no command substitution or pipeline
# runs while it scans, so it never finds its own helpers. Prints the main process it kept
# ("main: PID") and what it ended ("ended: PID..." or "ended: none") on stdout; exits 1 when
# processes are still running after the last round, after printing them as
# "survivors: PID:STARTTIME..." (the start time tells a process from a later one that reuses
# its PID; koinon-agentd then watches them, stuck.go), 2 when it cannot tell which process is
# the sandbox's main one, ending nothing.
set -u

uid=
while read -r k v _; do
  if [ "$k" = Uid: ]; then uid=$v; fi
done </proc/self/status
[ -n "$uid" ] || { echo "sweep: cannot read the user ID" >&2; exit 2; }

# Keep PID 1, this shell and its ancestors.
keep=" 1 $$ "
p=$$
while [ "$p" -gt 1 ]; do
  pp=
  while read -r k v _; do
    if [ "$k" = PPid: ]; then pp=$v; fi
  done <"/proc/$p/status"
  [ -n "$pp" ] || break
  keep="$keep$pp "
  p=$pp
done

# The sandbox's main process stays. Under OpenShell, PID 1 is the supervisor
# (openshell-sandbox, as the sandbox user too) and the main process is its first child: the
# command the sandbox was created with, launched once before the sandbox is ready, so before
# any exec. Ending it ends the sandbox (ReportMainProcessExit) and OpenShell refuses every
# exec after. Children of PID 1 started later are execs, or processes an exec left behind.
# The contract suite's Docker stand-in says so in PID 1's environment
# (KOINON_SANDBOX_STANDIN=1); its PID 1 is the main process. Any other PID 1 is not known, and
# the sweep refuses rather than guess.
main=
{ read -r comm </proc/1/comm; } 2>/dev/null || comm=
if [ "$comm" != openshell-sandb ]; then
  if grep -qaz '^KOINON_SANDBOX_STANDIN=1$' /proc/1/environ 2>/dev/null; then
    main=1
  else
    echo "sweep: unknown PID 1 (${comm:-unreadable}), neither the OpenShell supervisor nor the stand-in; cannot tell the sandbox's main process" >&2
    exit 2
  fi
else
  main=
  main_start=
  for d in /proc/[0-9]*; do
    { read -r line <"$d/stat"; } 2>/dev/null || continue
    # Fields after "pid (comm) ": state, ppid, ..., start time the 20th.
    # shellcheck disable=SC2086 # splitting the stat fields is the point
    set -- ${line##*") "}
    [ $# -ge 20 ] && [ "$2" = 1 ] || continue
    shift 19
    if [ -z "$main_start" ] || [ "$1" -lt "$main_start" ]; then
      main=${d#/proc/}
      main_start=$1
    fi
  done
  [ -n "$main" ] || { echo "sweep: the supervisor has no main process" >&2; exit 2; }
fi
keep="$keep$main "
echo "main: $main"

# scan sets found to this user's live processes outside keep.
scan() {
  found=
  for d in /proc/[0-9]*; do
    pid=${d#/proc/}
    case $keep in *" $pid "*) continue ;; esac
    u=
    st=
    { while read -r k v _; do
      case $k in
        Uid:) u=$v ;;
        State:) st=$v ;;
      esac
    done <"$d/status"; } 2>/dev/null || continue
    [ "$u" = "$uid" ] || continue
    [ "$st" = Z ] && continue
    found="$found $pid"
  done
}

# alive reports whether any of found is still running (not yet a zombie).
alive() {
  for pid in $found; do
    st=
    { while read -r k v _; do
      if [ "$k" = State: ]; then st=$v; fi
    done <"/proc/$pid/status"; } 2>/dev/null
    if [ -n "$st" ] && [ "$st" != Z ]; then return 0; fi
  done
  return 1
}

ended=
round=0
while :; do
  scan
  if [ -z "$found" ]; then
    echo "ended:${ended:- none}"
    exit 0
  fi
  round=$((round + 1))
  if [ "$round" -gt 5 ]; then
    sv=
    for pid in $found; do
      { read -r line <"/proc/$pid/stat"; } 2>/dev/null || continue
      # shellcheck disable=SC2086 # splitting the stat fields is the point
      set -- ${line##*") "}
      [ $# -ge 20 ] || continue
      shift 19
      sv="$sv $pid:$1"
    done
    echo "survivors:$sv"
    echo "still running:$found" >&2
    exit 1
  fi
  for pid in $found; do
    case "$ended " in *" $pid "*) ;; *) ended="$ended $pid" ;; esac
  done
  # TERM first, so a harness can save its state; KILL in later rounds.
  if [ "$round" -eq 1 ]; then sig=TERM; else sig=KILL; fi
  # shellcheck disable=SC2086 # found is a list of pids
  kill -s "$sig" $found 2>/dev/null
  i=0
  while alive && [ "$i" -lt 30 ]; do
    sleep 0.1
    i=$((i + 1))
  done
done
