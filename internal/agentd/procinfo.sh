# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=sh
#
# procinfo reads the processes koinon-agentd's sweep could not end (stuck.go). Its arguments are
# PID:STARTTIME pairs as sweep.sh prints them; for each that is still the same process and not a
# zombie it prints "PID|STATE|COMM|WCHAN" on stdout. A process that has gone, been replaced by
# another with the same PID, or only awaits its parent's wait (zombie) prints nothing. Nothing
# here signals or waits on a process; reading /proc does not block on one in uninterruptible
# sleep.
#
# KOINON_PROC names another proc root; tests use it.
set -u
proc=${KOINON_PROC:-/proc}
for a in "$@"; do
  pid=${a%%:*}
  want=${a#*:}
  line=
  { read -r line <"$proc/$pid/stat"; } 2>/dev/null || continue
  # shellcheck disable=SC2086 # splitting the stat fields is the point
  set -- ${line##*") "}
  [ $# -ge 20 ] || continue
  state=$1
  shift 19
  [ "$1" = "$want" ] || continue
  [ "$state" = Z ] && continue
  comm=
  { read -r comm <"$proc/$pid/comm"; } 2>/dev/null
  wchan=
  { read -r wchan <"$proc/$pid/wchan"; } 2>/dev/null
  echo "$pid|$state|$comm|$wchan"
done
exit 0
