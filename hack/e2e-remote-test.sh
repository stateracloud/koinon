#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/e2e-remote.sh without a real remote or e2e: `ssh` and `docker` are shims on PATH. The
# ssh shim runs the remote command in a local shell with HOME set to a scratch "remote home", so the
# push, the lock, the checkout, the run, the copy-back and the exit status are all exercised. The
# lock test needs flock (Linux): on macOS run this in the toolchain image:
#   docker run --rm -v "$PWD:/r" -w /r koinon-e2e-toolchain:<tag> hack/e2e-remote-test.sh

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/e2e-remote-test.XXXXXX")
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
  case "$*" in *--entrypoint*) exit 0 ;; esac # fix_owner
  for a in "$@"; do case $a in *:/out) out=${a%:/out} ;; *:/work) wsrc=${a%:/work} ;; esac; done
  echo "run src at $(git -C "$wsrc" rev-parse HEAD 2>/dev/null)" >>"$FAKE_LOG"
  echo "fake e2e ran"
  echo "| Phase | s |" >"$out/quickstart-timings.md"
  mkdir -p "$out/diagnostics"; echo x >"$out/diagnostics/pods.txt"
  if [ -n "${FAKE_EXEC_SLEEP:-}" ]; then exec sleep "$FAKE_EXEC_SLEEP"; fi
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
export KOINON_E2E_REMOTE=test@fake KOINON_E2E_REMOTE_WORK=work/t.git KOINON_E2E_LOCK="$tmp/lock"

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
check "the run ref is deleted from the work area" test -z "$(git --git-dir="$tmp/home/work/t.git" for-each-ref refs/e2e)"
check "the remote run directory is removed" test -z "$(/bin/ls "$tmp/home/e2e-runs" 2>/dev/null)"

# 2. Failure: the e2e's status is the script's, results still copied.
rc=0
FAKE_RC=7 hack/e2e-remote.sh --out "$tmp/out2" >"$tmp/log2" 2>&1 || rc=$?
check "exit 7 passes through" test "$rc" -eq 7
check "results copied on failure" test -f "$tmp/out2/quickstart-timings.md"

# 3. The lock, and two different commits of one branch: the second run waits, then tests its own commit.
sha1=$sha
echo two >"$tmp/repo/two.txt"
git -C "$tmp/repo" add -A
git -C "$tmp/repo" -c user.name=t -c user.email=t@example.invalid commit -q -m second
sha2=$(git -C "$tmp/repo" rev-parse HEAD)
: >"$FAKE_LOG"
git -C "$tmp/repo" checkout -q --detach "$sha1"
FAKE_SLEEP=3 hack/e2e-remote.sh --out "$tmp/out3a" >"$tmp/log3a" 2>&1 &
first=$!
sleep 1.5
git -C "$tmp/repo" checkout -q --detach "$sha2"
hack/e2e-remote.sh --out "$tmp/out3b" >"$tmp/log3b" 2>&1 &
second=$!
wait "$first" "$second"
check "the queued run printed that it is waiting" has 'waiting for the e2e lock' "$tmp/log3b"
check "both runs finished" test -f "$tmp/out3a/quickstart-timings.md" -a -f "$tmp/out3b/quickstart-timings.md"
check "the first run tested its commit" has "run src at $sha1" "$FAKE_LOG"
check "the second run tested its own commit" has "run src at $sha2" "$FAKE_LOG"

# 3b. SSH drops (the watched process dies): the stage stops the e2e, cleans up and frees the lock.
: >"$FAKE_LOG"
sleep 60 & fake_sshd=$!
start=$SECONDS
rc=0
KOINON_E2E_WATCH_PID=$fake_sshd FAKE_EXEC_SLEEP=60 hack/e2e-remote.sh --out "$tmp/out3c" >"$tmp/log3c" 2>&1 &
third=$!
sleep 3
kill "$fake_sshd"
wait "$third" || rc=$?
check "a dropped connection stops the run within 20 s" test $((SECONDS - start)) -lt 20 -a "$rc" -ne 0
check "the run container was removed" has 'rm -f koinon-e2e-' "$FAKE_LOG"
check "the lock is free again" flock -n "$tmp/lock" true

else
  echo "skip: no flock here (run in the toolchain image, see the header)"
fi

# 4. --local removes a stale cluster of its own name (we hold the lock), runs, and cleans up.
sed -i.bak 's|^ps) exit 0 ;;|ps) echo abc123; exit 0 ;;|' "$tmp/bin/docker"
: >"$FAKE_LOG"
rc=0
hack/e2e-remote.sh --local --out "$tmp/out4" >"$tmp/log4" 2>&1 || rc=$?
check "--local passes" test "$rc" -eq 0
check "--local removed the stale cluster" has 'rm -f -v abc123' "$FAKE_LOG"
check "--local mounts /var/run/docker.sock whatever DOCKER_HOST says" has '-v /var/run/docker.sock:/var/run/docker.sock' "$FAKE_LOG"

# 5. --local with the directory lock (macOS has no flock): waits for a live owner.
rm -rf "$tmp/lock.d"
mkdir "$tmp/lock.d"
sleep 4 & holder=$!
echo "$holder" >"$tmp/lock.d/pid"
rc=0
KOINON_E2E_FORCE_MKDIR_LOCK=1 hack/e2e-remote.sh --local --out "$tmp/out5" >"$tmp/log5" 2>&1 || rc=$?
check "the directory lock made --local wait" has 'waiting for the e2e lock' "$tmp/log5"
check "--local ran after the owner went away" test "$rc" -eq 0
check "the directory lock is released" test ! -d "$tmp/lock.d"

# 7. Only --local and --ssh exist: any other option is refused, exit 2.
rc=0
(unset KOINON_E2E_REMOTE; hack/e2e-remote.sh --no-wait >"$tmp/log14" 2>&1) || rc=$?
check "an unknown option exits 2" test "$rc" -eq 2
check "... naming the option" has 'unknown argument --no-wait' "$tmp/log14"
rc=0
(unset KOINON_E2E_REMOTE; hack/e2e-remote.sh --dry-run >"$tmp/log15" 2>&1) || rc=$?
check "no mode flag runs the local e2e (git archive plus docker run)" sh -c "test $rc -eq 0 && grep -q 'git archive' '$tmp/log15'"

# 7g. hack/e2e-status.sh on its own (the e2e-lab / go-test-lab commit-status contract). gh is a recorder; FAKE_GH_FAIL=1 makes the post fail.
cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "gh $*" >>"$FAKE_GH_LOG"
[[ -z ${FAKE_GH_FAIL:-} ]] || exit 1
exit 0
SH
chmod +x "$tmp/bin/gh"
export FAKE_GH_LOG="$tmp/gh.log"
status_sha=$sha
: >"$FAKE_GH_LOG"
rc=0; "$repo/hack/e2e-status.sh" "$status_sha" success "a run" "https://argo.example.invalid/w/x" >/dev/null 2>&1 || rc=$?
check "e2e-status posts a status" test "$rc" -eq 0 && check "... through gh api statuses with the context" has "statuses/$status_sha -f state=success -f context=e2e-lab -f description=a run -f target_url=https://argo.example.invalid/w/x" "$FAKE_GH_LOG"
rc=0; "$repo/hack/e2e-status.sh" "$status_sha" maybe x >/dev/null 2>&1 || rc=$?
check "e2e-status refuses a state that is not success or failure (64)" test "$rc" -eq 64
rc=0; "$repo/hack/e2e-status.sh" abc123 success x >/dev/null 2>&1 || rc=$?
check "e2e-status refuses a short sha (64)" test "$rc" -eq 64
: >"$FAKE_GH_LOG"
rc=0; KOINON_E2E_NO_STATUS=1 "$repo/hack/e2e-status.sh" "$status_sha" success x >/dev/null 2>&1 || rc=$?
check "e2e-status with KOINON_E2E_NO_STATUS posts nothing" test "$rc" -eq 0 -a ! -s "$FAKE_GH_LOG"
rc=0; FAKE_GH_FAIL=1 "$repo/hack/e2e-status.sh" "$status_sha" failure x >/dev/null 2>&1 || rc=$?
check "e2e-status exits 1 when the post fails" test "$rc" -eq 1

# 6. The toolchain image pins what hack/versions.env pins (kind, ko, helm, kubectl and their checksums).
df="$repo/hack/e2e-toolchain/Dockerfile"
check "toolchain image and workflows match hack/versions.env" "$repo/hack/versions-check.sh"
check "Go matches go.mod" test "$(sed -n 's/^ARG GO_VERSION=//p' "$df")" = "$(awk '/^go /{print $2; exit}' "$repo/go.mod")"
# The web variant (old issue 358) pins what the web suite uses: a mismatch makes the run download a browser.
wdf="$repo/hack/e2e-toolchain/web.Dockerfile" pkg="$repo/web/package.json"
check "web image: Node major matches CI's setup-node" \
  test "$(sed -n 's/^ARG NODE_VERSION=\([0-9]*\)\..*/\1/p' "$wdf")" = "$(sed -n 's/^ *node-version: *//p' "$repo/.github/workflows/ci.yml" | sort -u)"
check "web image: pnpm matches web/package.json" \
  test "pnpm@$(sed -n 's/^ARG PNPM_VERSION=//p' "$wdf")" = "$(jq -r .packageManager "$pkg")"
check "web image: Playwright matches web/package.json" \
  test "$(sed -n 's/^ARG PLAYWRIGHT_VERSION=//p' "$wdf")" = "$(jq -r '.devDependencies["@playwright/test"] // .dependencies["@playwright/test"]' "$pkg")"

[[ $fail -eq 0 ]] && echo "e2e-remote tests passed"
exit "$fail"
