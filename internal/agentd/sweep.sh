# SPDX-License-Identifier: Apache-2.0
#
# sweep ends every process in the sandbox that runs as this script's user, apart from the
# sandbox's main process and this script with its ancestors (the exec that runs it). koinon-agentd
# runs it before it starts a harness and after a run's harness has ended, so nothing the agent
# started in one run (a harness's daemon, a background job) lives into the next (sweep.go).
#
# POSIX sh with builtins and /proc only, so it works in every flavor image; no command
# substitution or pipeline, whose processes it would see. Prints what it ended on stdout; exits
# 1 when processes are still running after the last round.
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

# scan sets found to this user's live processes outside keep. The oldest of the user's
# processes is the sandbox's main process, started before any exec; it is kept too.
scan() {
  found=
  oldest=
  oldest_start=
  for d in /proc/[0-9]*; do
    pid=${d#/proc/}
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
    { read -r line <"$d/stat"; } 2>/dev/null || continue
    # Fields after "pid (comm) ": state is the first, start time the 20th.
    set -- ${line##*") "}
    [ $# -ge 20 ] || continue
    shift 19
    if [ -z "$oldest_start" ] || [ "$1" -lt "$oldest_start" ]; then
      oldest=$pid
      oldest_start=$1
    fi
    case $keep in *" $pid "*) continue ;; esac
    found="$found $pid"
  done
  if [ -n "$oldest" ]; then
    rest=
    for pid in $found; do
      [ "$pid" = "$oldest" ] || rest="$rest $pid"
    done
    found=$rest
  fi
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
    echo "still running:$found" >&2
    exit 1
  fi
  for pid in $found; do
    case "$ended " in *" $pid "*) ;; *) ended="$ended $pid" ;; esac
  done
  # TERM first, so a harness can save its state; KILL in later rounds.
  if [ "$round" -eq 1 ]; then sig=TERM; else sig=KILL; fi
  kill -s "$sig" $found 2>/dev/null
  i=0
  while alive && [ "$i" -lt 30 ]; do
    sleep 0.1
    i=$((i + 1))
  done
done
