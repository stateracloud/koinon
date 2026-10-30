#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/e2e-remote.sh without a real remote or e2e: `ssh` and `docker` are shims on PATH. The
# ssh shim runs the remote command in a local shell with HOME set to a scratch "remote home", so the
# push, the lock, the checkout, the run, the copy-back and the exit status are all exercised. The
# lock test needs flock (Linux): on macOS run this in the toolchain image:
#   docker run --rm -v "$PWD:/r" -w /r koinon-e2e-toolchain:<tag> hack/e2e-remote-test.sh
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; [[ -n ${KEEP_TMP:-} ]] || rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/home"
export REMOTE_HOME="$tmp/home" FAKE_LOG="$tmp/docker.log" PATH="$tmp/bin:$PATH"

# A repository to test from: the files the script reads, in a fresh commit.
mkdir -p "$tmp/repo/hack"
cp "$repo/go.mod" "$tmp/repo/"
cp "$repo/hack/e2e-remote.sh" "$tmp/repo/hack/"
cp -R "$repo/hack/e2e-toolchain" "$tmp/repo/hack/"
git -C "$tmp/repo" init -q -b main
git -C "$tmp/repo" add -A
git -C "$tmp/repo" -c user.name=t -c user.email=t@example.invalid commit -q -m test

# ssh <options> host command: run the command locally as the remote user.
cat >"$tmp/bin/ssh" <<'SH'
#!/usr/bin/env bash
while [[ $1 == -* ]]; do case $1 in -o) shift 2 ;; *) shift ;; esac; done
shift # host
cd "$REMOTE_HOME" && HOME="$REMOTE_HOME" exec bash -c "$1"
SH
# docker: records the call; `run` fakes the e2e (writes a report, exits $FAKE_RC).
cat >"$tmp/bin/docker" <<'SH'
#!/usr/bin/env bash
echo "docker $*" >>"$FAKE_LOG"
case $1 in
image) exit 0 ;;
ps) exit 0 ;;
run)
  for a in "$@"; do case $a in *:/out) out=${a%:/out} ;; esac; done
  echo "fake e2e ran" ; echo "| Phase | s |" >"$out/quickstart-timings.md"
  mkdir -p "$out/diagnostics"; echo x >"$out/diagnostics/pods.txt"
  sleep "${FAKE_SLEEP:-0}"
  exit "${FAKE_RC:-0}" ;;
*) exit 0 ;;
esac
SH
chmod +x "$tmp/bin/ssh" "$tmp/bin/docker"

fail=0
check() { # <description> <command...>: passes when the command succeeds
  local d=$1
  shift
  if "$@" >/dev/null 2>&1; then echo "ok: $d"; else echo "FAIL: $d" >&2; fail=1; fi
}
# shellcheck disable=SC2329 # called through check
has() { grep -q -- "$1" "$2"; } # has <pattern> <file>
export GIT_SSH_COMMAND="$tmp/bin/ssh"

cd "$tmp/repo"
sha=$(git rev-parse HEAD)
export KOINON_E2E_REMOTE=test@fake KOINON_E2E_REMOTE_WORK=work/t.git

if command -v flock >/dev/null; then
# 1. Success: pushed, run in the toolchain image with the socket mounted, results copied back.
rc=0
FAKE_RC=0 hack/e2e-remote.sh --out "$tmp/out1" >"$tmp/log1" 2>&1 || rc=$?
check "exit 0 passes through" test "$rc" -eq 0
check "the commit reached the remote work area" git --git-dir="$tmp/home/work/t.git" cat-file -e "$sha"
check "timings copied back" test -f "$tmp/out1/quickstart-timings.md"
check "diagnostics copied back" test -f "$tmp/out1/diagnostics/pods.txt"
check "docker run uses host networking" has 'run .*--network host' "$FAKE_LOG"
check "docker run mounts the socket" has 'docker.sock:/var/run/docker.sock' "$FAKE_LOG"
check "runs make test-e2e-kind" has 'make test-e2e-kind' "$FAKE_LOG"
check "the remote run directory is removed" test -z "$(/bin/ls "$tmp/home/e2e-runs" 2>/dev/null)"

# 2. Failure: the e2e's status is the script's, results still copied.
rc=0
FAKE_RC=7 hack/e2e-remote.sh --out "$tmp/out2" >"$tmp/log2" 2>&1 || rc=$?
check "exit 7 passes through" test "$rc" -eq 7
check "results copied on failure" test -f "$tmp/out2/quickstart-timings.md"

# 3. The lock: a second run waits while the first holds it.
if true; then
  FAKE_SLEEP=3 hack/e2e-remote.sh --out "$tmp/out3a" >"$tmp/log3a" 2>&1 &
  first=$!
  sleep 1.5
  hack/e2e-remote.sh --out "$tmp/out3b" >"$tmp/log3b" 2>&1 &
  second=$!
  wait "$first" "$second"
  check "the queued run printed that it is waiting" has 'waiting for the e2e lock' "$tmp/log3b"
  check "both runs finished" test -f "$tmp/out3a/quickstart-timings.md" -a -f "$tmp/out3b/quickstart-timings.md"
fi

else
  echo "skip: no flock here (run in the toolchain image, see the header)"
fi

# 4. --local refuses when the cluster exists (docker ps lists one).
sed -i.bak 's|^ps) exit 0 ;;|ps) echo abc123; exit 0 ;;|' "$tmp/bin/docker"
rc=0
hack/e2e-remote.sh --local --out "$tmp/out4" >"$tmp/log4" 2>&1 || rc=$?
check "--local refuses while a kind cluster exists" test "$rc" -eq 1
check "--local says why" has 'already exists' "$tmp/log4"

[[ $fail -eq 0 ]] && echo "e2e-remote tests passed"
exit "$fail"
