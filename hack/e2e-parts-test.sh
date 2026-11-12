#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# test/e2e/kind/parts.sh (#572), offline: when one parallel part fails, wait_all ends the whole process tree of the
# others. The slow part is a shell function (like install_openshell) that runs a stub "helm" which waits; a kill of
# the function's pid alone would leave the stub alive.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/e2e-parts-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
fail() { echo "e2e-parts-test: $*" >&2; exit 1; }

# The stub helm: records its pid, then waits (the real one waits up to 5 minutes). `sleep` is a child of the stub,
# so the stub is the process the test checks; it is a grandchild of the tracked function pid.
mkdir "$tmp/bin"
cat >"$tmp/bin/helm" <<STUB
#!/usr/bin/env bash
echo \$\$ >"$tmp/helm.pid"
sleep 60
STUB
chmod +x "$tmp/bin/helm"

# Runs the parts in a child bash, as up.sh does: one slow function part (helm), one part that fails after helm
# has started. A passing outcome: the child exits 1 and helm is gone.
cat >"$tmp/driver.sh" <<DRIVER
set -euo pipefail
export PATH="$tmp/bin:\$PATH"
source "$root/test/e2e/kind/parts.sh"
install_stub() { helm upgrade --install openshell; }
fail_late() { while [[ ! -s "$tmp/helm.pid" ]]; do :; done; exit 7; }
bg install_stub
bg fail_late
wait_all
echo "wait_all returned" >&2
DRIVER

# A zombie counts as dead: the killed stub is reparented to PID 1, and where PID 1 is not an init
# (some containers) nobody reaps it, so kill -0 still succeeds on it. The state comes from /proc/PID/stat
# (field 3, after the last ")") on Linux, which busybox and sandboxed hosts allow; ps -o stat= -p only where
# /proc/PID does not exist (macOS). If the state cannot be read, kill -0 stands: a live process is never
# reported dead (fail safe).
procdir=/proc
alive() {
  local st line
  kill -0 "$1" 2>/dev/null || return 1
  if [[ -d $procdir/$1 ]]; then
    # Unreadable (hidepid, another user) but still there: kill -0 stands. Gone: dead.
    line=$(cat "$procdir/$1/stat" 2>/dev/null) || { [[ -d $procdir/$1 ]] && return 0; return 1; }
    line=${line##*) }
    st=${line%% *}
  else
    st=$(ps -o stat= -p "$1" 2>/dev/null) || return 0
    st=${st//[[:space:]]/}
    [[ -n "$st" ]] || return 0
  fi
  [[ "$st" != Z* && "$st" != X* ]]
}

# alive() itself: live, zombie, missing pid, unusable ps.
sleep 30 &
lp=$!
alive "$lp" || fail "alive: a live process is reported dead"
# A zombie: the child exits and its parent (an exec'd sleep) never waits for it.
read -r zp zpar < <(bash -c 'sleep 0 & echo "$! $$"; exec sleep 30 >/dev/null')
for _ in $(seq 1 50); do alive "$zp" || break; sleep 0.1; done
# Where no state source works (sandboxed macOS ps), alive() fails safe and cannot see a zombie.
if [[ -d /proc/self ]] || ps -o stat= -p "$$" >/dev/null 2>&1; then
  if alive "$zp"; then fail "alive: a zombie is reported alive"; fi
else
  echo "e2e-parts-test: zombie check skipped: no process state source" >&2
fi
kill "$zpar" 2>/dev/null || true
kill "$lp"
wait "$lp" 2>/dev/null || true
if alive "$lp"; then fail "alive: a missing pid is reported alive"; fi
mkdir "$tmp/psbin"
printf '#!/bin/sh\nexit 1\n' >"$tmp/psbin/ps"
chmod +x "$tmp/psbin/ps"
sleep 30 &
lp=$!
(PATH="$tmp/psbin:$PATH" alive "$lp") || fail "alive: unusable ps makes a live process dead"
# A /proc entry whose stat cannot be read (a directory without the file): alive, not dead.
mkdir -p "$tmp/fakeproc/$lp"
(procdir=$tmp/fakeproc; alive "$lp") || fail "alive: an unreadable /proc stat makes a live process dead"
kill "$lp"
wait "$lp" 2>/dev/null || true

rc=0
bash "$tmp/driver.sh" 2>"$tmp/err" || rc=$?
[ "$rc" -eq 1 ] || fail "driver exit $rc, want 1 ($(cat "$tmp/err"))"
grep -q 'background part failed (exit 7): fail_late' "$tmp/err" || fail "failed part not named: $(cat "$tmp/err")"
[ -s "$tmp/helm.pid" ] || fail "stub helm never started"
hp=$(cat "$tmp/helm.pid")
if alive "$hp"; then
  kill "$hp" 2>/dev/null || true
  fail "stub helm (pid $hp) is still alive after the script returned"
fi

# An EXIT trap with kill_parts ends the tree too (the script is stopped some other way).
rm -f "$tmp/helm.pid"
cat >"$tmp/driver2.sh" <<DRIVER
set -euo pipefail
export PATH="$tmp/bin:\$PATH"
source "$root/test/e2e/kind/parts.sh"
trap kill_parts EXIT
install_stub() { helm upgrade --install openshell; }
bg install_stub
while [[ ! -s "$tmp/helm.pid" ]]; do :; done
exit 3
DRIVER
rc=0
bash "$tmp/driver2.sh" 2>/dev/null || rc=$?
[ "$rc" -eq 3 ] || fail "driver2 exit $rc, want 3"
hp=$(cat "$tmp/helm.pid")
if alive "$hp"; then
  kill "$hp" 2>/dev/null || true
  fail "stub helm (pid $hp) is still alive after the EXIT trap"
fi

# All parts succeed: wait_all returns.
cat >"$tmp/driver3.sh" <<DRIVER
set -euo pipefail
source "$root/test/e2e/kind/parts.sh"
bg true
bg true
wait_all
DRIVER
bash "$tmp/driver3.sh" || fail "successful parts failed"

echo "e2e-parts-test: ok"
