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

# 7. --argo (explicit; the default is --local): `curl` is a fake Argo Server and S3, the
# credentials file a fake. The lab settings are placeholders: nothing about a real lab is tracked. The bundle reaches S3, the least busy slot is picked from the template's
# enum, the run is followed and its results unpacked, and no credential is ever in curl's argv.
cp "$repo/hack/e2e-argo.sh" "$tmp/repo/hack/"
cp -R "$repo/hack/lab" "$tmp/repo/hack/"
export KOINON_LAB_CONFIG="$tmp/no-such-lab.env" # never the tester's own lab config
cat >"$tmp/light.workflow.json" <<'JSON'
{
  "//": ["comment key of the manifest"],
  "metadata": {"generateName": "koinon-light-"},
  "spec": {
    "entrypoint": "run",
    "arguments": {"parameters": [
      {"name": "target"}, {"name": "ref"}, {"name": "bundle"}, {"name": "agent"},
      {"name": "slot", "value": "light-1", "enum": ["light-1", "light-2"]}
    ]},
    "templates": [{"name": "run", "podSpecPatch": "{\"claim\": \"e2e-cache-{{workflow.parameters.slot}}\"}"}]
  }
}
JSON
git -C "$tmp/repo" checkout -q --detach "$sha"
argo_sha=$(git -C "$tmp/repo" rev-parse HEAD)
cat >"$tmp/creds.sh" <<'SH'
e2e_creds_open() { echo "creds open" >>"$FAKE_LOG"; }
e2e_creds_argo_token() { [[ -z ${FAKE_NO_TOKEN:-} ]] || return 1; echo koinon_test_not-a-real-token; }
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
PUT:*/e2e-bundles/*) cp "$upload" "$ARGO_DIR/last.bundle"; [[ -z $w ]] || printf '%s' "${FAKE_PUT_CODE:-200}" ;;
*/workflow-templates/*/koinon-gate) echo '{"spec":{"arguments":{"parameters":[{"name":"gate-set","enum":["go-test","agentd-ginkgo"]},{"name":"slot","enum":["gate-1","gate-2","gate-3","gate-4"]}]}}}' ;;
*/workflow-templates/*) echo '{"spec":{"arguments":{"parameters":[{"name":"slot","enum":["a","b","c"]}]}}}' ;;
*'/workflows/koinon-e2e?'*) if [[ -n ${FAKE_LIST:-} ]]; then echo "$FAKE_LIST"; else echo '{}'; fi ;;
POST:*/submit) printf '%s' "$body" >"$ARGO_DIR/submitted"
  if [[ -n $o ]]; then echo '{"metadata":{"name":"koinon-make-t1"}}' >"$o"; printf '%s' "${FAKE_SUBMIT_CODE:-200}"; else echo '{"metadata":{"name":"koinon-make-t1"}}'; fi ;;
POST:*/api/v1/workflows/koinon-e2e) printf '%s' "$body" >"$ARGO_DIR/created"; echo '{"metadata":{"name":"koinon-make-t1"}}' ;;
PUT:*/stop) echo stopped >>"$ARGO_DIR/stopped"; echo '{}' ;;
DELETE:*/workflows/koinon-e2e/koinon-make-t1) echo deleted >>"$ARGO_DIR/deleted"; echo '{}' ;;
*'/koinon-make-t1?fields='*)
  n=$(cat "$ARGO_DIR/polls" 2>/dev/null || echo 0); echo $((n + 1)) >"$ARGO_DIR/polls"
  if [[ -n ${FAKE_PENDING_UNSCHED:-} ]]; then echo '{"status":{"phase":"Running","nodes":{"p":{"type":"Pod","phase":"Pending","message":"Unschedulable: 0/3 nodes are available: 3 Insufficient cpu."}}}}'
  elif [[ -n ${FAKE_PENDING_LIMIT:-} ]]; then echo '{"status":{"phase":"Pending","message":"Workflow processing has been postponed because too many workflows are already running"}}'
  elif [[ -n ${FAKE_PENDING_LOCK:-} ]]; then echo '{"status":{"phase":"Pending","message":"Waiting for koinon-e2e/Mutex/e2e-gate-1 lock. Lock status: 0/1"}}'
  elif [[ -n ${FAKE_PENDING_PULL:-} ]]; then if [[ $n -lt 4 ]]; then echo '{"status":{"phase":"Running","nodes":{"p":{"type":"Pod","phase":"Pending","message":"ContainerCreating"}}}}'; else echo '{"status":{"phase":"Succeeded"}}'; fi
  elif [[ -n ${FAKE_PENDING_POD:-} ]]; then echo '{"status":{"phase":"Running","nodes":{"p":{"type":"Pod","phase":"Pending","message":"exceeded quota"}}}}'
  elif [[ -n ${FAKE_PENDING:-} ]]; then echo '{"status":{"phase":"Pending","message":"Waiting for e2e-gate-1 lock"}}'
  elif [[ $n -lt 1 ]]; then echo '{"status":{"phase":"Running"}}'; else echo "{\"status\":{\"phase\":\"${FAKE_PHASE:-Succeeded}\"}}"; fi ;;
*/koinon-make-t1/log?*) echo '{"result":{"content":"== 12:00:00Z prerequisites (kind/up.sh)"}}' ;;
*/workflows/koinon-e2e/koinon-make-t1) echo '{"status":{"nodes":{"koinon-make-t1":{"id":"koinon-make-t1","type":"Pod","outputs":{"artifacts":[{"name":"results","s3":{"key":"koinon-make-t1/koinon-make-t1/results.tgz"}}]}}}}}' ;;
*/e2e-artifacts/koinon-make-t1/koinon-make-t1/results.tgz | */artifact-files/*)
  echo "$url" >>"$ARGO_DIR/results-from"
  d=$(mktemp -d "${TMPDIR:-/tmp}/fake-results.XXXXXX"); mkdir -p "$d/out"; echo "| Phase | s |" >"$d/out/quickstart-timings.md"
  echo "${FAKE_RESULT_LINE:-target=test-e2e-kind ref=x rc=${FAKE_RESULT_RC:-0} seconds=1}" >"$d/out/result.txt"
  tar -czf "$o" -C "$d" out; rm -rf "$d" ;;
*) echo "fake curl: unexpected $method $url" >&2; exit 22 ;;
esac
SH
chmod +x "$tmp/bin/curl"
argo_env() { # <dir>: a fresh fake server state
  rm -rf "$1"; mkdir -p "$1"
  export ARGO_DIR=$1 FAKE_CFG=$1/cfg KOINON_E2E_ARGO_CREDS="$tmp/creds.sh" KOINON_E2E_ARGO_POLL=0
  export KOINON_E2E_ARGO_SERVER=https://argo.example.invalid KOINON_E2E_BUNDLE_URL=https://s3.example.invalid/e2e-bundles
  export KOINON_E2E_RESULTS_URL=https://s3.example.invalid/e2e-artifacts KOINON_E2E_LIGHT_MANIFEST="$tmp/light.workflow.json"
}

argo_env "$tmp/argo1"
: >"$FAKE_LOG"
rc=0
(unset KOINON_E2E_REMOTE
  FAKE_LIST='{"items":[{"spec":{"arguments":{"parameters":[{"name":"slot","value":"a"}]}}}]}' \
    KOINON_E2E_AGENT='agent koinon/1' hack/e2e-remote.sh --argo --out "$tmp/out7" >"$tmp/log7" 2>&1) || rc=$?
check "--argo passes" test "$rc" -eq 0
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

argo_env "$tmp/argo2b"
rc=0
(unset KOINON_E2E_REMOTE; FAKE_PHASE=Failed FAKE_RESULT_RC=0 hack/e2e-remote.sh --argo --out "$tmp/out8b" >"$tmp/log8b" 2>&1) || rc=$?
check "a run Failed after its target passed (result.txt rc=0) counts as passed" test "$rc" -eq 0
check "... and says so" has 'after its target passed' "$tmp/log8b"

argo_env "$tmp/argo3"
: >"$FAKE_LOG"
rc=0
(unset KOINON_E2E_REMOTE; hack/e2e-remote.sh --argo --no-wait --slot c >"$tmp/log9" 2>&1) || rc=$?
check "--no-wait returns after the submit" test "$rc" -eq 0 -a ! -e "$tmp/argo3/polls"
check "--slot is passed through" has 'slot=c' "$tmp/argo3/submitted"
check "--no-wait prints how to follow" has 'hack/e2e-remote.sh --argo --follow koinon-make-t1' "$tmp/log9"
check "--no-wait keeps the login (the token must outlive the submit)" sh -c "! grep -q 'creds closed' '$FAKE_LOG'"

argo_env "$tmp/argo4"
rc=0
(unset KOINON_E2E_REMOTE; cd "$tmp" && "$tmp/repo/hack/e2e-remote.sh" --argo --repo "$tmp/repo" --no-wait >"$tmp/log10" 2>&1) || rc=$?
check "--repo tests that checkout from outside it" sh -c "test $rc -eq 0 && grep -q 'ref=$argo_sha' '$tmp/argo4/submitted'"

# 7b. --lane light: the manifest goes in as an inline Workflow with its parameters filled in, on the
# least busy light slot; light-run.sh refuses a target that is not a light one.
argo_env "$tmp/argo5"
rc=0
(unset KOINON_E2E_REMOTE
  FAKE_LIST='{"items":[{"spec":{"arguments":{"parameters":[{"name":"slot","value":"light-1"}]}}}]}' \
    hack/e2e-remote.sh --argo --lane light --out "$tmp/out11" >"$tmp/log11" 2>&1) || rc=$?
check "--lane light passes" test "$rc" -eq 0
check "--lane light creates an inline Workflow, not a template submit" test -f "$tmp/argo5/created" -a ! -e "$tmp/argo5/submitted"
param() { jq -r --arg n "$1" '.workflow.spec.arguments.parameters[] | select(.name == $n) | .value' "$tmp/argo5/created"; }
check "--lane light defaults to test-go" test "$(param target)" = test-go
check "--lane light submits the commit" test "$(param ref)" = "$argo_sha"
check "--lane light picks the least busy light slot" test "$(param slot)" = light-2
check "the manifest's comment key is not sent" test "$(jq 'has("//") or (.workflow | has("//"))' "$tmp/argo5/created")" = false
check "the manifest's podSpecPatch is sent as is" sh -c "jq -r '.workflow.spec.templates[0].podSpecPatch' '$tmp/argo5/created' | grep -q 'e2e-cache-{{workflow.parameters.slot}}'"
check "light-run.sh refuses a target that is not a light one" sh -c "! OUT='$tmp/out12' '$repo/hack/lab/light-run.sh' test-e2e-kind 2>/dev/null"

# 7c. The lab is opt-in (#508): the default mode is --local, the --argo options need --argo, and --argo
# without lab settings exits 2 naming them instead of reaching for any lab.
argo_env "$tmp/argo6"
rc=0
(unset KOINON_E2E_REMOTE; hack/e2e-remote.sh --dry-run >"$tmp/log13" 2>&1) || rc=$?
check "no mode flag runs the local e2e" test "$rc" -eq 0
check "... which is the git archive plus docker run, not a submit" has 'git archive' "$tmp/log13"
rc=0
(unset KOINON_E2E_REMOTE; hack/e2e-remote.sh --no-wait >"$tmp/log14" 2>&1) || rc=$?
check "--no-wait without --argo exits 2" test "$rc" -eq 2
check "... and says to add --argo" has 'add --argo' "$tmp/log14"
rc=0
(unset KOINON_E2E_REMOTE KOINON_E2E_ARGO_SERVER KOINON_E2E_BUNDLE_URL KOINON_E2E_RESULTS_URL KOINON_E2E_ARGO_CREDS
  hack/e2e-remote.sh --argo --dry-run >"$tmp/log15" 2>&1) || rc=$?
check "--argo without lab settings exits 2" test "$rc" -eq 2
check "... naming the server variable" has 'KOINON_E2E_ARGO_SERVER' "$tmp/log15"
check "... and the config file" has 'lab.env' "$tmp/log15"
check "... and no curl was made" test ! -s "$tmp/argo6/cfg"
rc=0
(unset KOINON_E2E_REMOTE KOINON_E2E_LIGHT_MANIFEST; hack/e2e-remote.sh --argo --lane light --dry-run >"$tmp/log16" 2>&1) || rc=$?
check "--lane light without a manifest exits 2" test "$rc" -eq 2
check "... naming KOINON_E2E_LIGHT_MANIFEST" has 'KOINON_E2E_LIGHT_MANIFEST' "$tmp/log16"
# The settings can come from the untracked config file; the environment wins over it.
cat >"$tmp/lab.env" <<ENV
KOINON_E2E_ARGO_SERVER=https://argo.example.invalid
KOINON_E2E_BUNDLE_URL=https://from-file.example.invalid/e2e-bundles
KOINON_E2E_RESULTS_URL=https://s3.example.invalid/e2e-artifacts
KOINON_E2E_ARGO_CREDS=$tmp/creds.sh
ENV
rc=0
(unset KOINON_E2E_REMOTE KOINON_E2E_ARGO_SERVER KOINON_E2E_BUNDLE_URL KOINON_E2E_RESULTS_URL KOINON_E2E_ARGO_CREDS
  KOINON_LAB_CONFIG="$tmp/lab.env" hack/e2e-remote.sh --argo --dry-run >"$tmp/log17" 2>&1) || rc=$?
check "the lab config file supplies the settings" test "$rc" -eq 0
check "... including the bundle URL" has 'from-file.example.invalid' "$tmp/log17"
rc=0
(unset KOINON_E2E_REMOTE; KOINON_E2E_BUNDLE_URL=https://from-env.example.invalid/e2e-bundles KOINON_LAB_CONFIG="$tmp/lab.env" \
  hack/e2e-remote.sh --argo --dry-run >"$tmp/log18" 2>&1) || rc=$?
check "the environment wins over the file" has 'from-env.example.invalid' "$tmp/log18"

# 7d. --gate <set> (#512): the koinon-gate template with gate-set, ref, bundle, agent and slot, the
# labels, the exit contract (0 pass, the gate's rc, 75 no slot, 76 NOT RUN) and the pending give-up.
gate() { # <dir> <result line> [env...]: one gate run; sets rc
  local d=$1 line=$2
  shift 2
  argo_env "$d"
  rc=0
  (unset KOINON_E2E_REMOTE; env FAKE_RESULT_LINE="$line" "$@" hack/e2e-remote.sh --argo --gate go-test --out "$d/out" >"$d/log" 2>&1) || rc=$?
}
gate "$tmp/gate1" "target=go-test ref=x rc=0 seconds=1 verdict=pass" FAKE_PHASE=Succeeded \
  FAKE_LIST='{"items":[{"spec":{"arguments":{"parameters":[{"name":"slot","value":"gate-1"}]}}}]}' KOINON_E2E_AGENT='2d024d34-local'
check "--gate pass exits 0" test "$rc" -eq 0
check "--gate submits the koinon-gate template" has '"resourceName":"koinon-gate"' "$tmp/gate1/submitted"
check "--gate passes gate-set, ref, bundle, agent" sh -c "grep -q 'gate-set=go-test' '$tmp/gate1/submitted' && grep -q 'ref=$argo_sha' '$tmp/gate1/submitted' && grep -q 'bundle=koinon-' '$tmp/gate1/submitted' && grep -q 'agent=2d024d34-local' '$tmp/gate1/submitted'"
check "--gate picks a slot with no unfinished run" has 'slot=gate-2' "$tmp/gate1/submitted"
check "--gate passes no labels (the template sets lane, target and agent)" test "$(jq -r '.submitOptions.labels // ""' "$tmp/gate1/submitted")" = ""
check "--gate selects busy slots by the lane=gate label" has 'labelSelector=lab.example.invalid/lane=gate' "$FAKE_LOG"
check "--gate follows the log" has 'logOptions.follow=true' "$FAKE_LOG"
check "--gate fetches \$OUT" test -f "$tmp/gate1/out/result.txt"
gate "$tmp/gate2" "target=go-test ref=x rc=1 seconds=1 verdict=fail" FAKE_PHASE=Failed
check "--gate fail passes the gate's rc through" test "$rc" -eq 1
gate "$tmp/gate2b" "target=go-test ref=x rc=7 seconds=1 verdict=fail" FAKE_PHASE=Failed
check "--gate fail exits with the gate's own status" test "$rc" -eq 7
gate "$tmp/gate3" "target=go-test ref=x rc=3 seconds=1 verdict=notrun" FAKE_PHASE=Failed
check "--gate verdict=notrun exits 76" test "$rc" -eq 76
check "... and says NOT RUN" has 'NOT RUN' "$tmp/gate3/log"
gate "$tmp/gate3b" "target=go-test ref=x rc=3 seconds=1 verdict=notrun" FAKE_PHASE=Succeeded
check "--gate verdict=notrun exits 76 even when the pod succeeded" test "$rc" -eq 76
gate "$tmp/gate4" "" FAKE_PENDING=1 KOINON_E2E_GATE_PENDING=1
check "--gate Pending past the limit exits 75" test "$rc" -eq 75
check "... without stopping the run" test ! -e "$tmp/gate4/stopped"
check "... and deleting it" test -s "$tmp/gate4/deleted"
argo_env "$tmp/gate4b"; : >"$FAKE_LOG"
gate "$tmp/gate4b" "" FAKE_PENDING_POD=1 KOINON_E2E_GATE_PENDING=1
check "--gate Running with every pod Pending past the limit exits 75" test "$rc" -eq 75
check "... without stopping the run" test ! -e "$tmp/gate4b/stopped"
check "... and deleting it" test -s "$tmp/gate4b/deleted"
check "... without following the log" sh -c "! grep -q 'logOptions.follow' '$FAKE_LOG'"
check "... naming the reason (quota) and the workflow" sh -c "grep -q 'koinon-make-t1 is still Pending.*reason: quota' '$tmp/gate4b/log'"
argo_env "$tmp/gate4c"; : >"$FAKE_LOG"
gate "$tmp/gate4c" "" FAKE_PENDING_UNSCHED=1 KOINON_E2E_GATE_PENDING=1
check "--gate with an Unschedulable pod past the limit exits 75" test "$rc" -eq 75
check "... naming the reason (unschedulable) and the workflow" sh -c "grep -q 'koinon-make-t1 is still Pending.*reason: unschedulable' '$tmp/gate4c/log'"
check "... deleting the run without stopping it" sh -c "test ! -e '$tmp/gate4c/stopped' && test -s '$tmp/gate4c/deleted'"
argo_env "$tmp/gate4e"; : >"$FAKE_LOG"
gate "$tmp/gate4e" "" FAKE_PENDING_LIMIT=1 KOINON_E2E_GATE_PENDING=1
check "--gate postponed by the namespace limit exits 75" test "$rc" -eq 75
check "... naming the reason (namespace-limit) and the workflow" sh -c "grep -q 'koinon-make-t1 is still Pending.*reason: namespace-limit' '$tmp/gate4e/log'"
check "... deleting the run without stopping it" sh -c "test ! -e '$tmp/gate4e/stopped' && test -s '$tmp/gate4e/deleted'"
argo_env "$tmp/gate4f"; : >"$FAKE_LOG"
gate "$tmp/gate4f" "" FAKE_PENDING_LOCK=1 KOINON_E2E_GATE_PENDING=1
check "--gate waiting on the slot mutex exits 75" test "$rc" -eq 75
check "... naming the reason (slot), not quota or namespace-limit" sh -c "grep -q 'koinon-make-t1 is still Pending.*reason: slot' '$tmp/gate4f/log'"
check "... deleting the run without stopping it" sh -c "test ! -e '$tmp/gate4f/stopped' && test -s '$tmp/gate4f/deleted'"
argo_env "$tmp/gate4d"; : >"$FAKE_LOG"
gate "$tmp/gate4d" "" FAKE_PENDING_PULL=1 KOINON_E2E_GATE_PENDING=1 KOINON_E2E_POLL=1
check "--gate with a pod in ContainerCreating does not give up" test "$rc" -ne 75
check "... and does not stop the run" test ! -e "$tmp/gate4d/stopped"
argo_env "$tmp/gate6"
rc=0
(unset KOINON_E2E_REMOTE
  FAKE_LIST='{"items":[{"spec":{"arguments":{"parameters":[{"name":"slot","value":"gate-1"},{"name":"slot","value":"gate-2"},{"name":"slot","value":"gate-3"},{"name":"slot","value":"gate-4"}]}}}]}' \
    hack/e2e-remote.sh --argo --gate go-test >"$tmp/gate6/log" 2>&1) || rc=$?
check "--gate with every slot busy exits 75 without submitting" test "$rc" -eq 75 -a ! -e "$tmp/gate6/submitted"
rc=0
(unset KOINON_E2E_REMOTE; FAKE_PUT_CODE=503 hack/e2e-remote.sh --argo --gate go-test >"$tmp/gate6/log4" 2>&1) || rc=$?
check "--gate with a failing bundle upload exits 75 without submitting" test "$rc" -eq 75 -a ! -e "$tmp/gate6/submitted"
rc=0
rm -f "$tmp/gate6/last.bundle"
(unset KOINON_E2E_REMOTE; FAKE_NO_TOKEN=1 hack/e2e-remote.sh --argo --gate go-test >"$tmp/gate6/log5" 2>&1) || rc=$?
check "--gate without an Argo token exits 75 without uploading" test "$rc" -eq 75 -a ! -e "$tmp/gate6/last.bundle"
rc=0
(unset KOINON_E2E_REMOTE; hack/e2e-remote.sh --argo --gate 'x;rm' --dry-run >"$tmp/gate6/log2" 2>&1) || rc=$?
check "--gate refuses a gate set that is not a plain name" test "$rc" -eq 2
rc=0
(unset KOINON_E2E_REMOTE; hack/e2e-remote.sh --argo --gate chart-check >"$tmp/gate6/log6" 2>&1) || rc=$?
check "--gate refuses a set the lab template does not accept (exit 2)" test "$rc" -eq 2
check "... naming what it accepts" has 'go-test agentd-ginkgo' "$tmp/gate6/log6"
check "... with no upload and no submit" test ! -e "$tmp/gate6/last.bundle" -a ! -e "$tmp/gate6/submitted"
rc=0
(unset KOINON_E2E_REMOTE; hack/e2e-remote.sh --argo --gate go-test --dry-run >"$tmp/gate6/log7" 2>&1) || rc=$?
check "--gate dry-run exits 0 under /bin/bash" test "$rc" -eq 0
check "... naming the koinon-gate template" has 'koinon-e2e/koinon-gate' "$tmp/gate6/log7"
rc=0
(unset KOINON_E2E_REMOTE; hack/e2e-remote.sh --argo --dry-run >"$tmp/gate6/log8" 2>&1) || rc=$?
check "the kind dry-run names the koinon-make template" has 'koinon-e2e/koinon-make' "$tmp/gate6/log8"
rc=0
(unset KOINON_E2E_REMOTE; hack/e2e-remote.sh --gate go-test --dry-run >"$tmp/gate6/log3" 2>&1) || rc=$?
check "--gate without --argo exits 2" test "$rc" -eq 2

# 7e. hack/lab/gate-run.sh: the gate-set enum, the command each set runs, and the verdict mapping
# (a go gate's 3 is notrun, read from KOINON_RC_FILE because make turns it into 2).
cat >"$tmp/bin/make" <<'SH'
#!/usr/bin/env bash
echo "make $* PROCS=${PROCS:-} RC_FILE=${KOINON_RC_FILE:+set}" >>"$FAKE_MAKE_LOG"
[[ -z ${FAKE_GO_RC:-} || -z ${KOINON_RC_FILE:-} ]] || echo "$FAKE_GO_RC" >"$KOINON_RC_FILE"
exit "${FAKE_MAKE_RC:-0}"
SH
chmod +x "$tmp/bin/make"
export FAKE_MAKE_LOG="$tmp/make.log"
gate_run() { # <gate set> [env...]: rc and $tmp/gr/result.txt
  local gs=$1
  shift
  rm -rf "$tmp/gr"
  : >"$FAKE_MAKE_LOG"
  rc=0
  (cd "$tmp/repo" && env OUT="$tmp/gr" "$@" "$repo/hack/lab/gate-run.sh" "$gs" >"$tmp/gr.log" 2>&1) || rc=$?
}
gate_run go-test
check "gate-run go-test passes" test "$rc" -eq 0
check "... running make test-go with PROCS set explicitly" has 'make test-go PROCS=4' "$FAKE_MAKE_LOG"
check "... and the result line has verdict=pass" has 'rc=0 .*verdict=pass' "$tmp/gr/result.txt"
check "... with the commit and seconds" sh -c "grep -q 'ref=$argo_sha' '$tmp/gr/result.txt' && grep -q 'seconds=' '$tmp/gr/result.txt'"
check "... and driver.log" test -f "$tmp/gr/driver.log"
gate_run agentd-ginkgo
check "gate-run agentd-ginkgo runs the agentd suite" has 'make test-go-suite SUITE=./internal/agentd' "$FAKE_MAKE_LOG"
gate_run go-test FAKE_MAKE_RC=2 FAKE_GO_RC=3
check "gate-run: go-test.sh's 3 (through make's 2) is notrun" has 'rc=3 .*verdict=notrun' "$tmp/gr/result.txt"
check "... and exits 3" test "$rc" -eq 3
gate_run go-test FAKE_MAKE_RC=2 FAKE_GO_RC=1
check "gate-run: go-test.sh's 1 is fail" has 'rc=1 .*verdict=fail' "$tmp/gr/result.txt"
gate_run go-test FAKE_MAKE_RC=2
check "gate-run: a make failure without a status file is fail" has 'rc=2 .*verdict=fail' "$tmp/gr/result.txt"
gate_run chart-check FAKE_MAKE_RC=3
check "gate-run: a non-Go gate's 3 is a plain failure, not notrun" has 'rc=3 .*verdict=fail' "$tmp/gr/result.txt"
gate_run test-e2e-kind
check "gate-run refuses a gate set that is not in the enum (exit 2)" test "$rc" -eq 2 -a ! -s "$FAKE_MAKE_LOG"
gate_run 'go-test; id'
check "... or an injected one" test "$rc" -eq 2 -a ! -s "$FAKE_MAKE_LOG"

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
