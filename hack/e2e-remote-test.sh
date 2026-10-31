#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/e2e-remote.sh without a real remote or e2e: `ssh`, `docker` and `curl` are shims on PATH. The
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

# 7. --argo (the default without KOINON_E2E_REMOTE): `curl` is a fake Argo Server and S3, the
# credentials file a fake. The bundle reaches S3, the least busy slot is picked from the template's
# enum, the run is followed and its results unpacked, and no credential is ever in curl's argv.
cp "$repo/hack/e2e-argo.sh" "$tmp/repo/hack/"
cp -R "$repo/hack/lab" "$tmp/repo/hack/"
git -C "$tmp/repo" checkout -q --detach "$sha"
argo_sha=$(git -C "$tmp/repo" rev-parse HEAD)
cat >"$tmp/creds.sh" <<'SH'
e2e_creds_open() { echo "creds open" >>"$FAKE_LOG"; }
e2e_creds_argo_token() { echo koinon_test_not-a-real-token; }
e2e_creds_s3() { echo koinon_test_id:koinon_test_not-a-real-secret; }
e2e_creds_results() { echo koinon_test_reader:koinon_test_not-a-real-reader; }
e2e_creds_close() { echo "creds closed" >>"$FAKE_LOG"; }
SH
cat >"$tmp/bin/curl" <<'SH'
#!/usr/bin/env bash
cat >>"$FAKE_CFG" # the -K - config: credentials arrive here, never in argv
echo "curl $*" >>"$FAKE_LOG"
method=GET url="" body="" o="" w="" upload=""
while [[ $# -gt 0 ]]; do
  case $1 in
  -X) method=$2; shift ;;
  --data-binary) body=$2; shift ;;
  -o) o=$2; shift ;;
  -w) w=$2; shift ;;
  -T) method=PUT; upload=$2; shift ;;
  -H | -K | --aws-sigv4) shift ;;
  http*) url=$1 ;;
  esac
  shift
done
case $method:$url in
PUT:*/e2e-bundles/*) cp "$upload" "$ARGO_DIR/last.bundle"; [[ -z $w ]] || printf 200 ;;
*/workflow-templates/*) echo '{"spec":{"arguments":{"parameters":[{"name":"slot","enum":["a","b","c"]}]}}}' ;;
*'/workflows/koinon-e2e?'*) if [[ -n ${FAKE_LIST:-} ]]; then echo "$FAKE_LIST"; else echo '{}'; fi ;;
POST:*/submit) printf '%s' "$body" >"$ARGO_DIR/submitted"; echo '{"metadata":{"name":"koinon-make-t1"}}' ;;
POST:*/api/v1/workflows/koinon-e2e) printf '%s' "$body" >"$ARGO_DIR/created"; echo '{"metadata":{"name":"koinon-make-t1"}}' ;;
PUT:*/stop) echo stopped >>"$ARGO_DIR/stopped"; echo '{}' ;;
*'/koinon-make-t1?fields='*)
  n=$(cat "$ARGO_DIR/polls" 2>/dev/null || echo 0); echo $((n + 1)) >"$ARGO_DIR/polls"
  if [[ $n -lt 1 ]]; then echo '{"status":{"phase":"Running"}}'; else echo "{\"status\":{\"phase\":\"${FAKE_PHASE:-Succeeded}\"}}"; fi ;;
*/koinon-make-t1/log?*) echo '{"result":{"content":"== 12:00:00Z prerequisites (kind/up.sh)"}}' ;;
*/workflows/koinon-e2e/koinon-make-t1) echo '{"status":{"nodes":{"koinon-make-t1":{"id":"koinon-make-t1","type":"Pod","outputs":{"artifacts":[{"name":"results","s3":{"key":"koinon-make-t1/koinon-make-t1/results.tgz"}}]}}}}}' ;;
*/e2e-artifacts/koinon-make-t1/koinon-make-t1/results.tgz | */artifact-files/*)
  echo "$url" >>"$ARGO_DIR/results-from"
  d=$(mktemp -d); mkdir -p "$d/out"; echo "| Phase | s |" >"$d/out/quickstart-timings.md"
  echo "target=test-e2e-kind ref=x rc=${FAKE_RESULT_RC:-0} seconds=1" >"$d/out/result.txt"
  tar -czf "$o" -C "$d" out; rm -rf "$d" ;;
*) echo "fake curl: unexpected $method $url" >&2; exit 22 ;;
esac
SH
chmod +x "$tmp/bin/curl"
argo_env() { # <dir>: a fresh fake server state
  rm -rf "$1"; mkdir -p "$1"
  export ARGO_DIR=$1 FAKE_CFG=$1/cfg KOINON_E2E_ARGO_CREDS="$tmp/creds.sh" KOINON_E2E_ARGO_POLL=0
}

argo_env "$tmp/argo1"
: >"$FAKE_LOG"
rc=0
(unset KOINON_E2E_REMOTE
  FAKE_LIST='{"items":[{"spec":{"arguments":{"parameters":[{"name":"slot","value":"a"}]}}}]}' \
    KOINON_E2E_AGENT='agent koinon/1' hack/e2e-remote.sh --out "$tmp/out7" >"$tmp/log7" 2>&1) || rc=$?
check "--argo is the default and passes" test "$rc" -eq 0
check "the bundle holds the commit" sh -c "git clone -q --no-checkout '$tmp/argo1/last.bundle' '$tmp/argo1/clone' && git -C '$tmp/argo1/clone' cat-file -e $argo_sha"
check "no temporary branch is left" test -z "$(git -C "$tmp/repo" for-each-ref 'refs/heads/e2e-bundle-*')"
check "submits the commit" has "ref=$argo_sha" "$tmp/argo1/submitted"
check "submits the kind e2e" has 'target=test-e2e-kind' "$tmp/argo1/submitted"
check "picks the least busy slot of the template's enum" has 'slot=b' "$tmp/argo1/submitted"
check "the agent label is a valid label value" has 'agent=agent-koinon-1' "$tmp/argo1/submitted"
check "prints the run's stage lines" has '== 12:00:00Z prerequisites' "$tmp/log7"
check "results unpacked" test -f "$tmp/out7/quickstart-timings.md"
check "results come from the results bucket, not the Argo Server" has '/e2e-artifacts/koinon-make-t1/' "$tmp/argo1/results-from"
check "the results key reached curl only on stdin" has 'koinon_test_not-a-real-reader' "$tmp/argo1/cfg"
check "the credentials are closed after the run" has 'creds closed' "$FAKE_LOG"
check "the token reached curl only on stdin" has 'koinon_test_not-a-real-token' "$tmp/argo1/cfg"
check "no credential in curl's argv" sh -c "! grep -q 'koinon_test_not-a-real' '$FAKE_LOG'"

argo_env "$tmp/argo2"
rc=0
(unset KOINON_E2E_REMOTE; FAKE_PHASE=Failed FAKE_RESULT_RC=7 hack/e2e-remote.sh --argo --out "$tmp/out8" >"$tmp/log8" 2>&1) || rc=$?
check "--argo passes the e2e's exit status through" test "$rc" -eq 7
check "results unpacked on failure" test -f "$tmp/out8/quickstart-timings.md"

argo_env "$tmp/argo3"
: >"$FAKE_LOG"
rc=0
(unset KOINON_E2E_REMOTE; hack/e2e-remote.sh --no-wait --slot c >"$tmp/log9" 2>&1) || rc=$?
check "--no-wait returns after the submit" test "$rc" -eq 0 -a ! -e "$tmp/argo3/polls"
check "--slot is passed through" has 'slot=c' "$tmp/argo3/submitted"
check "--no-wait prints how to follow" has 'hack/e2e-remote.sh --argo --follow koinon-make-t1' "$tmp/log9"
check "--no-wait keeps the login (the token must outlive the submit)" sh -c "! grep -q 'creds closed' '$FAKE_LOG'"

argo_env "$tmp/argo4"
rc=0
(unset KOINON_E2E_REMOTE; cd "$tmp" && "$tmp/repo/hack/e2e-remote.sh" --repo "$tmp/repo" --no-wait >"$tmp/log10" 2>&1) || rc=$?
check "--repo tests that checkout from outside it" sh -c "test $rc -eq 0 && grep -q 'ref=$argo_sha' '$tmp/argo4/submitted'"

# 7b. --lane light: the manifest goes in as an inline Workflow with its parameters filled in, on the
# least busy light slot; light-run.sh refuses a target that is not a light one.
argo_env "$tmp/argo5"
rc=0
(unset KOINON_E2E_REMOTE
  FAKE_LIST='{"items":[{"spec":{"arguments":{"parameters":[{"name":"slot","value":"light-1"}]}}}]}' \
    hack/e2e-remote.sh --lane light --out "$tmp/out11" >"$tmp/log11" 2>&1) || rc=$?
check "--lane light passes" test "$rc" -eq 0
check "--lane light creates an inline Workflow, not a template submit" test -f "$tmp/argo5/created" -a ! -e "$tmp/argo5/submitted"
param() { jq -r --arg n "$1" '.workflow.spec.arguments.parameters[] | select(.name == $n) | .value' "$tmp/argo5/created"; }
check "--lane light defaults to test-go" test "$(param target)" = test-go
check "--lane light submits the commit" test "$(param ref)" = "$argo_sha"
check "--lane light picks the least busy light slot" test "$(param slot)" = light-2
check "the manifest's comment key is not sent" test "$(jq 'has("//") or (.workflow | has("//"))' "$tmp/argo5/created")" = false
check "the slot's Block volume is in podSpecPatch" sh -c "jq -r '.workflow.spec.templates[0].podSpecPatch' '$tmp/argo5/created' | grep -q 'e2e-cache-{{workflow.parameters.slot}}'"
check "light-run.sh refuses a target that is not a light one" sh -c "! OUT='$tmp/out12' '$repo/hack/lab/light-run.sh' test-e2e-kind 2>/dev/null"

# 6. The toolchain image pins what the workflow pins.
wf="$repo/.github/workflows/e2e-kind.yml" df="$repo/hack/e2e-toolchain/Dockerfile"
pin() { sed -n "s/^ *$1: *//p" "$wf" | head -1; }
check "kind version matches the workflow" has "ARG KIND_VERSION=$(pin KIND_VERSION)\$" "$df"
check "kind sha256 matches the workflow" has "ARG KIND_SHA256_amd64=$(pin KIND_SHA256)\$" "$df"
check "ko version matches the workflow" has "ARG KO_VERSION=$(pin KO_VERSION)\$" "$df"
check "ko sha256 matches the workflow" has "ARG KO_SHA256_amd64=$(pin KO_SHA256)\$" "$df"
helm=$(sed -n '/setup-helm/,/version:/s/^ *version: *//p' "$wf" | head -1)
check "helm version matches the workflow" has "ARG HELM_VERSION=$helm\$" "$df"
check "Go matches go.mod" test "$(sed -n 's/^ARG GO_VERSION=//p' "$df")" = "$(awk '/^go /{print $2; exit}' "$repo/go.mod")"
# The web variant (#358) pins what the web suite uses: a mismatch makes the run download a browser.
wdf="$repo/hack/e2e-toolchain/web.Dockerfile" pkg="$repo/web/package.json"
check "web image: Node major matches CI's setup-node" \
  test "$(sed -n 's/^ARG NODE_VERSION=\([0-9]*\)\..*/\1/p' "$wdf")" = "$(sed -n 's/^ *node-version: *//p' "$repo/.github/workflows/ci.yml" | sort -u)"
check "web image: pnpm matches web/package.json" \
  test "pnpm@$(sed -n 's/^ARG PNPM_VERSION=//p' "$wdf")" = "$(jq -r .packageManager "$pkg")"
check "web image: Playwright matches web/package.json" \
  test "$(sed -n 's/^ARG PLAYWRIGHT_VERSION=//p' "$wdf")" = "$(jq -r '.devDependencies["@playwright/test"] // .dependencies["@playwright/test"]' "$pkg")"

[[ $fail -eq 0 ]] && echo "e2e-remote tests passed"
exit "$fail"
