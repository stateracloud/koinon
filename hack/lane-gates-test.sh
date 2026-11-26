#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline self-test of hack/lane-gates.sh (koinon#321): stub preflight, ci-changes and make, then
# check the per-gate lines, the verdicts (pass, fail, notrun with a reason) and the exit codes.
# No Docker, no network, no real gate runs.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
script=$PWD/hack/lane-gates.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/lane-gates-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
export TMPDIR=$tmp
# The envtest gates (#1296): every case gets found assets and a lock directory that does not exist,
# so the stubs below never see the envtest-assets call. The envtest cases further down unset them.
mkdir -p "$tmp/assets"
export KOINON_ENVTEST_ASSETS=$tmp/assets LANE_GATES_ENVTEST_LOCKDIR=$tmp/no-lock-dir

# Stub preflight: --list prints $STUB_GATES ("name: reason" lines); --only <g> behaves as $STUB_<g>.
cat >"$tmp/preflight" <<'EOF'
#!/usr/bin/env bash
case $1 in
--base) shift 2 ;;
esac
if [[ $1 == --list ]]; then
	for g in $STUB_GATES; do echo "$g: because"; done
	[[ -n ${STUB_LIST_RC:-} ]] && { echo "list exploded" >&2; exit "$STUB_LIST_RC"; }
	[[ -z ${STUB_EMPTY:-} ]] && echo "gitleaks: always"
	echo "e2e: informational, always"
	exit 0
fi
g=$2
[[ -z ${STUB_SLEEP:-} ]] || sleep "$STUB_SLEEP"
v=STUB_$g
case ${!v:-ok} in
ok) echo "$g ok 1s" ;;
fail) echo "$g FAIL 1s"; echo "boom"; exit 1 ;;
incomplete) echo "$g INCOMPLETE 1s (skipped: docker)"; echo "INCOMPLETE (missing docker)"; exit 3 ;;
incomplete_vc) echo "$g INCOMPLETE 1s (skipped: volumecopy)"; echo "INCOMPLETE (missing volumecopy)"; exit 3 ;;
realfail) # full log: failed spec early, EPERM text in the tail, like Koinon's own sandbox tests
	printf -- '--- FAIL: TestSandbox (0.1s)\nline\nline\nline\nline\nline\nopen /x: operation not permitted\ngo-test verdict: FAILED: x\n' >"$STUB_DIR/$g.inner"
	echo "$g FAIL 1s log=$STUB_DIR/$g.inner"; tail -n 5 "$STUB_DIR/$g.inner" | sed 's/^/  | /'; exit 1 ;;
skip) echo "$g skip (pnpm not installed)" ;;
esac
EOF
# Stub ci-changes: prints $STUB_JOBS as name=true.
cat >"$tmp/changes" <<'EOF'
#!/usr/bin/env bash
for j in $STUB_JOBS; do echo "$j=true"; done
EOF
# Stub make: target behaviour from $STUB_MAKE_<target>.
cat >"$tmp/make" <<'EOF'
#!/usr/bin/env bash
case ${STUB_MAKE:-ok} in
ok) echo "made $1" ;;
fail) echo "assertion failed in $1"; exit 1 ;;
nodocker) echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock"; exit 1 ;;
sandbox) echo "open /x: operation not permitted"; exit 1 ;;
dockerperm) echo "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock"; exit 1 ;;
network) echo "go: github.com/x/y@v1: Get \"https://proxy.golang.org/x\": proxyconnect tcp: CONNECT tunnel failed, response 403"; exit 1 ;;
netallow) echo "failed downloading schema at https://schemas.example.invalid/x.json: not in this command's allowed_domains ? re-run the command"; exit 1 ;;
tlsproxy) echo '#30 0.212 curl: (60) SSL certificate problem: self-signed certificate in certificate chain'; exit 1 ;;
ratelimit) echo 'ERROR: failed to solve: unexpected status from HEAD request to https://registry.example.invalid/v2/x/manifests/1: 429 Too Many Requests'; exit 1 ;;
agentproxy) echo 'govulncheck: fetching vulnerabilities: Get "https://vuln.example.invalid/index.json": Forbidden (agent proxy: no rule or network setting allows this host)'; exit 1 ;;
networkfail) printf -- '--- FAIL: TestX (0.1s)\ndial tcp: lookup proxy.golang.org: no such host\n'; exit 1 ;;
esac
EOF
chmod +x "$tmp/preflight" "$tmp/changes" "$tmp/make"

fails=0 total=0
# A base with a committed difference to HEAD (the root commit); HEAD itself is the empty-diff case.
# The script works on the git repository it is started in, so the cases run in a scratch repo with
# two commits: the real checkout may be shallow or a detached merge commit (CI), where "the committed
# diff" is whatever that checkout happens to hold.
repo="$tmp/repo"
mkdir "$repo"
git -C "$repo" init -q -b main
git -C "$repo" config user.name t
git -C "$repo" config user.email t@example.invalid
echo a >"$repo/f" && git -C "$repo" add f && git -C "$repo" commit -q -m one
nonempty_base=$(git -C "$repo" rev-parse HEAD)
echo b >"$repo/f" && git -C "$repo" commit -q -am two
# run <env assignments...>: sets out and rc.
run() {
	out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make" "$@" bash "$script" "$nonempty_base" 2>&1)
	rc=$?
}
indent() { while IFS= read -r l; do echo "  | $l"; done <<<"$out"; }
expect() { # <name> <want rc> <pattern>...
	local name=$1 want=$2 p
	shift 2
	total=$((total + 1))
	if [[ $rc -ne $want ]]; then
		echo "FAIL: $name: exit $rc, want $want"; indent; fails=$((fails + 1)); return
	fi
	for p in "$@"; do
		if ! grep -Eq -- "$p" <<<"$out"; then
			echo "FAIL: $name: no line matches /$p/"; indent; fails=$((fails + 1)); return
		fi
	done
}

# No committed change: the path gates are notrun and ci-changes.sh is not asked (it would say everything).
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes-bad" LANE_GATES_MAKE="$tmp/make" STUB_GATES="go" bash "$script" HEAD 2>&1); rc=$?
expect "empty committed diff" 3 '^gate=paths rc=0 .*verdict=notrun reason="no committed change"' '^gate=go .*verdict=pass'

# Every gate passes; e2e is not run; the Docker-free machine path is not taken.
run STUB_GATES="go tidy" STUB_JOBS=""
expect "all pass" 0 '^gate=go rc=0 seconds=[0-9]+ verdict=pass log=' '^gate=tidy rc=0 seconds=[0-9]+ verdict=pass' '^lane-gates ok'
grep -q 'gate=e2e' <<<"$out" && { echo "FAIL: e2e must not run"; fails=$((fails + 1)); }

# A failing gate: fail verdict, exit 1, later gates still run.
run STUB_GATES="go tidy" STUB_go=fail
expect "fail" 1 '^gate=go rc=1 seconds=[0-9]+ verdict=fail log=' '^gate=tidy .*verdict=pass' '^lane-gates FAIL'

# exit 3 from a gate (missing requirement): notrun with the reason, never fail.
run STUB_GATES="go" STUB_go=incomplete
expect "incomplete" 3 '^gate=go rc=3 .*verdict=notrun reason="missing docker"' '^lane-gates INCOMPLETE'

# The chart smoke probe skipped (a runtime that does not copy into a volume): notrun with the reason.
run STUB_GATES="chartsmoke" STUB_chartsmoke=incomplete_vc
expect "chartsmoke volumecopy" 3 '^gate=chartsmoke rc=3 .*verdict=notrun reason="missing volumecopy"' '^lane-gates INCOMPLETE'

# A real failure whose tail mentions EPERM: classified from the inner log, which is the logged path.
run STUB_DIR="$tmp" STUB_GATES="go" STUB_go=realfail
expect "inner log decides" 1 "^gate=go rc=1 .*verdict=fail log=$tmp/go.inner"

# A tool that is not installed: notrun, exit 3.
run STUB_GATES="web" STUB_web=skip
expect "skip" 3 '^gate=web rc=0 .*verdict=notrun reason="pnpm not installed"'

# A failure and a notrun together: still exit 1.
run STUB_GATES="go web" STUB_go=fail STUB_web=skip
expect "fail beats notrun" 1 'gate=go .*verdict=fail' 'gate=web .*verdict=notrun'

# Heavy CI jobs from ci-changes.sh: pass, fail, and machine problems.
# A stub docker on PATH decides whether the Docker-needing jobs (chartsmoke, harnesses) can run.
mkdir "$tmp/bin-up" "$tmp/bin-down"
printf '#!/bin/sh\nexit 0\n' >"$tmp/bin-up/docker"
printf '#!/bin/sh\nexit 0\n' >"$tmp/bin-up/helm"
printf '#!/bin/sh\nexit 0\n' >"$tmp/bin-up/go"
printf '#!/bin/sh\nexit 1\n' >"$tmp/bin-down/docker"
chmod +x "$tmp/bin-up/helm" "$tmp/bin-up/go" "$tmp/bin-up/docker" "$tmp/bin-down/docker"
# Escape and chartsmoke need helm/go on PATH; put $tmp/bin-up first so the cases run offline
# without the host's tools (#349). bin-down's docker (exit 1) must precede bin-up's docker.
run PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="chartsmoke harnesses escape"
expect "jobs pass" 0 '^gate=chartsmoke .*verdict=pass' '^gate=harnesses .*verdict=pass' '^gate=escape .*verdict=pass'
run PATH="$tmp/bin-down:$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="chartsmoke escape"
expect "no docker daemon" 3 '^gate=chartsmoke rc=0 .*verdict=notrun reason="docker unavailable"' '^gate=escape .*verdict=pass'
run PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=fail
expect "job fail" 1 '^gate=escape rc=1 .*verdict=fail'
run PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=nodocker
expect "job no docker" 3 '^gate=escape rc=1 .*verdict=notrun reason="docker unavailable"'
run PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=sandbox
expect "job sandbox" 3 '^gate=escape rc=1 .*verdict=notrun reason="sandbox denial"'
run PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=dockerperm
expect "job docker socket denied" 3 '^gate=escape rc=1 .*verdict=notrun reason="docker unavailable"'
run PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=network
expect "job network denied" 3 '^gate=escape rc=1 .*verdict=notrun reason="network denied"'
run PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=netallow
expect "job allowed_domains denial" 3 '^gate=escape rc=1 .*verdict=notrun reason="network denied"'
run PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=agentproxy
expect "job agent proxy refusal" 3 '^gate=escape rc=1 .*verdict=notrun reason="network denied"'
run PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=tlsproxy
expect "job TLS interception" 3 '^gate=escape rc=1 .*verdict=notrun reason="network denied \(TLS interception\)"'
run PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=ratelimit
expect "job registry rate limit" 3 '^gate=escape rc=1 .*verdict=notrun reason="registry rate limit"'
run PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=networkfail
expect "a real failure beats a network line" 1 '^gate=escape rc=1 .*verdict=fail'

# A failing log that merely says "Sandbox admission policy" is a failure, not a machine problem.
cat >"$tmp/make-sbx" <<'EOF2'
#!/usr/bin/env bash
echo "=== RUN   TestEscape/Sandbox_admission_policy"
echo "--- FAIL: TestEscape (0.1s)"
echo "FAIL	github.com/stateracloud/koinon/internal/sandbox"
exit 1
EOF2
chmod +x "$tmp/make-sbx"
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make-sbx" STUB_GATES="" STUB_JOBS="escape" PATH="$tmp/bin-up:$PATH" bash "$script" "$nonempty_base" 2>&1); rc=$?
expect "sandbox word is a failure" 1 '^gate=escape rc=1 .*verdict=fail'

# ci-changes turns on go while preflight does not select it: the go gate still runs.
run STUB_GATES="tidy" STUB_JOBS="go"
expect "ci job go maps to gate go" 0 '^gate=go rc=0 .*verdict=pass' '^gate=tidy .*verdict=pass'
run STUB_GATES="go" STUB_JOBS="go"
[[ $(grep -c '^gate=go ' <<<"$out") -eq 1 ]] || { echo "FAIL: go gate must run once"; fails=$((fails + 1)); }

# Real failure plus a Docker skip message in the same log: still a failure.
cat >"$tmp/make-mixed" <<'EOF2'
#!/usr/bin/env bash
echo "SKIP: docker is not available (testenv)"
echo "--- FAIL: TestReal (0.1s)"
exit 1
EOF2
chmod +x "$tmp/make-mixed"
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make-mixed" STUB_GATES="" STUB_JOBS="escape" PATH="$tmp/bin-up:$PATH" bash "$script" "$nonempty_base" 2>&1); rc=$?
expect "failure beats docker text" 1 '^gate=escape rc=1 .*verdict=fail'

# Helm / gitleaks failure markers beat EPERM text (#349).
cat >"$tmp/make-helmerr" <<'EOF2'
#!/usr/bin/env bash
echo "Error: INSTALLATION FAILED: open /tmp/x: operation not permitted"
exit 1
EOF2
chmod +x "$tmp/make-helmerr"
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make-helmerr" STUB_GATES="" STUB_JOBS="escape" PATH="$tmp/bin-up:$PATH" bash "$script" "$nonempty_base" 2>&1); rc=$?
expect "helm Error beats sandbox denial" 1 '^gate=escape rc=1 .*verdict=fail'
cat >"$tmp/make-gitleaks" <<'EOF2'
#!/usr/bin/env bash
echo "open /x: operation not permitted"
echo "WRN leaks found: 1"
exit 1
EOF2
chmod +x "$tmp/make-gitleaks"
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make-gitleaks" STUB_GATES="" STUB_JOBS="escape" PATH="$tmp/bin-up:$PATH" bash "$script" "$nonempty_base" 2>&1); rc=$?
expect "gitleaks leaks found beats sandbox denial" 1 '^gate=escape rc=1 .*verdict=fail'
cat >"$tmp/make-bufnet" <<'EOF2'
#!/usr/bin/env bash
echo "Failure: the server hosted at that remote is unavailable."
exit 1
EOF2
chmod +x "$tmp/make-bufnet"
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make-bufnet" STUB_GATES="" STUB_JOBS="escape" PATH="$tmp/bin-up:$PATH" bash "$script" "$nonempty_base" 2>&1); rc=$?
expect "buf remote unavailable is network denied" 3 '^gate=escape rc=1 .*verdict=notrun reason="network denied"'

# A second run of the same gate holding that gate's pid file: fail with a reason that says why (#349, #990).
cat >"$tmp/preflight-busy" <<'EOF2'
#!/usr/bin/env bash
case $1 in --base) shift 2 ;; esac
if [[ $1 == --list ]]; then echo "gitleaks: always"; echo "e2e: informational, always"; exit 0; fi
echo "preflight: the gitleaks gate is already in progress for /tmp/x (run 12345); stop it with hack/preflight.sh --stop (one pid file per gate: runs of other gates do not conflict)" >&2
exit 2
EOF2
chmod +x "$tmp/preflight-busy"
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight-busy" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make" STUB_JOBS="" bash "$script" "$nonempty_base" 2>&1); rc=$?
expect "parallel preflight is named" 1 '^gate=gitleaks rc=2 .*verdict=fail reason="preflight already running this gate in this worktree"'

# The gates run in parallel (#990): two slow gates take about one gate's time, no gate reports `already
# in progress`, and the lines come out in plan order whatever the finishing order.
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make" STUB_GATES="go web" STUB_JOBS="" STUB_SLEEP=4 bash "$script" "$nonempty_base" 2>&1); rc=$?
expect "two gates run at once" 0 '^gate=go .*verdict=pass' '^gate=web .*verdict=pass' '^lane-gates ok'
if grep -q 'already in progress' <<<"$out"; then echo "FAIL: two parallel gates: a gate reported already in progress"; fails=$((fails + 1)); fi
total=$((total + 1))
t0=$SECONDS
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make" STUB_GATES="go web chart" STUB_JOBS="" STUB_SLEEP=3 bash "$script" "$nonempty_base" 2>&1); rc=$?
wall=$((SECONDS - t0))
total=$((total + 1))
if [[ $wall -ge 9 ]]; then echo "FAIL: four 3 s gates took ${wall}s, they did not run in parallel"; fails=$((fails + 1)); fi
order=$(sed -n 's/^gate=\([a-z]*\) .*/\1/p' <<<"$out" | tr '\n' ' ')
total=$((total + 1))
if [[ $order != "go web chart gitleaks " ]]; then echo "FAIL: gate lines are out of plan order: $order"; fails=$((fails + 1)); fi
# LANE_GATES_JOBS=1 is the old behaviour: one at a time.
t0=$SECONDS
out=$(cd "$repo" && env LANE_GATES_JOBS=1 LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make" STUB_GATES="go web" STUB_JOBS="" STUB_SLEEP=2 bash "$script" "$nonempty_base" 2>&1); rc=$?
total=$((total + 1))
if [[ $((SECONDS - t0)) -lt 6 ]]; then echo "FAIL: LANE_GATES_JOBS=1 ran three 2 s gates in under 6 s"; fails=$((fails + 1)); fi
# The two gates that rewrite the tree (tidy, generate) run after the others, alone.
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make" STUB_GATES="tidy go generate" STUB_JOBS="" bash "$script" "$nonempty_base" 2>&1); rc=$?
order=$(sed -n 's/^gate=\([a-z]*\) .*/\1/p' <<<"$out" | tr '\n' ' ')
total=$((total + 1))
if [[ $order != "go gitleaks tidy generate " ]]; then echo "FAIL: tidy and generate do not run last, in order: $order"; fails=$((fails + 1)); fi

# ci-changes failing: the command fails loudly, never passes.
cat >"$tmp/changes-bad" <<'EOF2'
#!/usr/bin/env bash
echo "fatal: no merge base" >&2
exit 128
EOF2
chmod +x "$tmp/changes-bad"
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes-bad" LANE_GATES_MAKE="$tmp/make" STUB_GATES="go" bash "$script" "$nonempty_base" 2>&1); rc=$?
expect "ci-changes fails" 1 'ci-changes.*failed|changes-bad failed' 'no merge base'

# A missing tool for a heavy job is notrun with the tool named.
run LANE_GATES_MISSING_TOOLS="helm" STUB_GATES="" STUB_JOBS="escape"
expect "helm missing" 3 '^gate=escape .*verdict=notrun reason="helm not installed"'
run LANE_GATES_MISSING_TOOLS="go" PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="chartsmoke harnesses escape"
expect "go missing" 3 '^gate=chartsmoke .*verdict=notrun reason="go not installed"' '^gate=harnesses .*verdict=notrun reason="go not installed"' '^gate=escape .*verdict=notrun reason="go not installed"'

# preflight --list crashing is never "ok".
run STUB_GATES="go" STUB_LIST_RC=2
expect "list crash" 1 'list.* failed \(exit 2\)' 'list exploded'
grep -q 'lane-gates ok' <<<"$out" && { echo "FAIL: list crash said ok"; fails=$((fails + 1)); }
# An empty gate list (gitleaks is always selected) means something broke.
run STUB_GATES="" STUB_EMPTY=1
expect "empty list" 1 'selected no gate'
# A CI job with no local gate is notrun, exit 3.
run STUB_GATES="go" STUB_JOBS="mystery"
expect "unknown job" 3 '^gate=mystery .*verdict=notrun reason="no local gate for CI job mystery"' '^lane-gates INCOMPLETE'
# A pnpm sandbox denial next to a vitest failure is a failure.
cat >"$tmp/make-vitest" <<'EOF2'
#!/usr/bin/env bash
echo "pnpm: operation not permitted"
echo " FAIL  src/app.test.ts > renders"
echo " Test Files  1 failed (1)"
exit 1
EOF2
chmod +x "$tmp/make-vitest"
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make-vitest" STUB_GATES="go" STUB_JOBS="escape" PATH="$tmp/bin-up:$PATH" bash "$script" "$nonempty_base" 2>&1); rc=$?
expect "vitest failure beats sandbox denial" 1 '^gate=escape rc=1 .*verdict=fail'
# The pnpm store lock denied by a sandbox is notrun, although pnpm prints "Error:" (#1242).
cat >"$tmp/make-pnpmlock" <<'EOF2'
#!/usr/bin/env bash
echo "Error: ERR_PNPM_STORE_DIR_OPEN_OPERATION_LOCK"
echo "  Failed to open the store operation lock: Operation not permitted (os error 1)"
exit 1
EOF2
chmod +x "$tmp/make-pnpmlock"
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make-pnpmlock" STUB_GATES="go" STUB_JOBS="escape" PATH="$tmp/bin-up:$PATH" bash "$script" "$nonempty_base" 2>&1); rc=$?
expect "pnpm store lock denial" 3 '^gate=escape rc=1 .*verdict=notrun reason="sandbox denial \(pnpm store lock\)"'
# The pnpm lock signature next to a real test failure is still a fail (failure markers win).
cat >"$tmp/make-pnpmlock-fail" <<'EOF2'
#!/usr/bin/env bash
echo " FAIL  src/app.test.ts > renders"
echo "Error: ERR_PNPM_STORE_DIR_OPEN_OPERATION_LOCK"
echo "  Failed to open the store operation lock: Operation not permitted (os error 1)"
exit 1
EOF2
chmod +x "$tmp/make-pnpmlock-fail"
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make-pnpmlock-fail" STUB_GATES="go" STUB_JOBS="escape" PATH="$tmp/bin-up:$PATH" bash "$script" "$nonempty_base" 2>&1); rc=$?
expect "pnpm store lock with a real failure" 1 '^gate=escape rc=1 .*verdict=fail'
# Hooks print a warning.
run STUB_GATES="go"
expect "hook warning" 0 'warning: test hook LANE_GATES_PREFLIGHT is set'

# --start / --wait (#654): detach, a verdict file, the same lines and exit codes, 75 while running.
startrun() { # <env assignments...>: sets vfile
	vfile=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make" "$@" bash "$script" --start "$nonempty_base" 2>/dev/null)
}
waitrun() { out=$(cd "$repo" && bash "$script" --wait "$@" 2>&1); rc=$?; }
startrun STUB_GATES="go tidy"
total=$((total + 1))
[[ -f $vfile ]] || { echo "FAIL: --start printed no verdict file ($vfile)"; fails=$((fails + 1)); }
waitrun "$vfile" --max 30
expect "start/wait pass" 0 '^gate=go rc=0 .*verdict=pass' '^gate=tidy .*verdict=pass' '^lane-gates ok'
waitrun "$vfile" --max 0
expect "wait again after the end" 0 '^lane-gates ok'
startrun STUB_GATES="go tidy" STUB_go=fail
waitrun "$vfile" --max 30
expect "start/wait fail" 1 '^gate=go .*verdict=fail' '^gate=tidy .*verdict=pass' '^lane-gates FAIL'
startrun STUB_GATES="go" STUB_go=incomplete
waitrun "$vfile" --max 30
expect "start/wait notrun" 3 '^gate=go .*verdict=notrun' '^lane-gates INCOMPLETE'
startrun STUB_GATES="go" STUB_SLEEP=6
waitrun "$vfile" --max 0
expect "wait while running" 75 'still running'
waitrun "$vfile" --max 60
expect "wait finishes the slow run" 0 '^gate=go .*verdict=pass'
# A run killed without an exit code is a failure, not "still running" for ever.
mkdir "$tmp/dead"
: >"$tmp/dead/verdict"
echo 999999 >"$tmp/dead/pid"
waitrun "$tmp/dead/verdict" --max 5
expect "wait on a vanished run" 1 'gone and left no exit code'
waitrun "$tmp/none/verdict"
expect "wait on a missing file" 2 'no verdict file'
waitrun
expect "wait without a file" 2 'needs the verdict file'
waitrun "$vfile" --max x
expect "wait bad max" 2 '--max needs a number'
out=$(cd "$repo" && bash "$script" --start no-such-ref-xyz 2>&1); rc=$?
expect "start unknown base" 2 'unknown base'

# --start by a relative path from a subdirectory: the run must find the script (the first cut did not).
mkdir -p "$repo/hack" "$repo/sub"
cp hack/lane-gates.sh hack/paths-lib.sh hack/gate-reuse-lib.sh hack/gate-state-lib.sh hack/paths.rules "$repo/hack/"
vfile=$(cd "$repo/sub" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make" STUB_GATES="go" bash ../hack/lane-gates.sh --start "$nonempty_base" 2>/dev/null)
waitrun "$vfile" --max 30
expect "start by a relative path from a subdirectory" 0 '^gate=go .*verdict=pass' '^lane-gates ok'
# A run that dies with a code that is no verdict (here 127) is a failure with its tail, not that code.
mkdir "$tmp/odd"
echo "bash: boom: command not found" >"$tmp/odd/verdict"
echo 127 >"$tmp/odd/rc"
waitrun "$tmp/odd/verdict"
expect "wait maps a non-verdict exit code to 1" 1 'exited 127 before it had a verdict' 'command not found'

# Tool state (#1296). A stub make that prints the state each gate sees, and the envtest assets.
cat >"$tmp/make-state" <<'EOF2'
#!/usr/bin/env bash
target=${!#} # lane-gates passes flags (-s --no-print-directory) before the target
case $target in
envtest-assets)
	[[ -z ${STUB_ASSETS_MARK:-} ]] || : >"$STUB_ASSETS_MARK"
	case ${STUB_ASSETS:-ok} in
	ok) echo "$STUB_ASSETS_DIR" ;;
	sandbox) echo "mkdir /home/x/.local/share/kubebuilder-envtest: operation not permitted" >&2; exit 1 ;;
	broken) echo "make: *** No rule to make target" >&2; exit 2 ;;
	esac ;;
*) echo "target=$target buildx=${BUILDX_CONFIG:-unset} pnpm=${KOINON_PNPM_STORE:-unset} buf=${BUF_CACHE_DIR:-unset} docker=${DOCKER_CONFIG:-unset} assets=${KOINON_ENVTEST_ASSETS:-unset}" ;;
esac
EOF2
chmod +x "$tmp/make-state"
staterun() { # <env assignments...>: one run with the state stub; sets out and rc
	out=$(cd "$repo" && env -u KOINON_ENVTEST_ASSETS LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make-state" STUB_GATES="" STUB_ASSETS_DIR="$tmp/assets" PATH="$tmp/bin-up:$PATH" "$@" bash "$script" "$nonempty_base" 2>&1)
	rc=$?
}
gate_log() { # <gate>: the log path of the gate's line in $out
	sed -n "s/^gate=$1 .* log=\(.*\)$/\1/p" <<<"$out"
}
mkdir -p "$tmp/home/.docker/buildx/instances"
echo mybuilder >"$tmp/home/.docker/buildx/current"
echo '{"Name":"mybuilder"}' >"$tmp/home/.docker/buildx/instances/mybuilder"
mkdir -p "$tmp/home/.docker/buildx/activity" "$tmp/home/.docker/contexts"
echo '{"auths":{}}' >"$tmp/home/.docker/config.json"
echo seed >"$tmp/home/.docker/.token_seed" && echo x >"$tmp/home/.docker/.foo"
staterun STUB_JOBS="escape harnesses" HOME="$tmp/home" BUILDX_CONFIG= KOINON_PNPM_STORE= BUF_CACHE_DIR= DOCKER_CONFIG=
expect "state: gates pass with the tool state in sandbox paths" 0 '^gate=escape .*verdict=pass' '^gate=harnesses .*verdict=pass'
st=$(cat "$(gate_log harnesses)")
total=$((total + 1))
case $st in
*"buildx=$tmp/koinon-preflight/"*"/state/buildx pnpm=$tmp/koinon-pnpm-store buf=$tmp/koinon-buf-cache docker=$tmp/koinon-preflight/"*"/state/docker assets=$tmp/assets"*) ;;
*) echo "FAIL: state: buildx, pnpm, buf and envtest paths: $st"; fails=$((fails + 1)) ;;
esac
bx=${st#*buildx=}; bx=${bx%% *}
total=$((total + 1))
if [[ $(cat "$bx/current" 2>/dev/null) != mybuilder || ! -f $bx/instances/mybuilder || -e $bx/activity ]]; then
	echo "FAIL: state: the buildx copy has the builder selection and no activity: $(/bin/ls -A "$bx" 2>&1)"; fails=$((fails + 1))
fi
dc=${st#*docker=}; dc=${dc%% *}
total=$((total + 1))
if [[ $(readlink "$dc/config.json") != "$tmp/home/.docker/config.json" || $(readlink "$dc/contexts") != "$tmp/home/.docker/contexts" || -e $dc/buildx || -e $dc/.token_seed || ! -L $dc/.foo || ! -w $dc ]]; then
	echo "FAIL: state: the docker config dir links config.json, contexts and dotfiles, not buildx or .token_seed: $(/bin/ls -lA "$dc" 2>&1)"; fails=$((fails + 1))
fi
# A builder removed after the last run goes from the copy too.
rm -f "$tmp/home/.docker/buildx/current" && rm -rf "$tmp/home/.docker/buildx/instances"
rm -f "$tmp/home/.docker/config.json"
staterun STUB_JOBS="harnesses" HOME="$tmp/home" BUILDX_CONFIG= KOINON_PNPM_STORE= DOCKER_CONFIG=
total=$((total + 1))
if [[ -e $bx/current || -e $bx/instances ]]; then
	echo "FAIL: state: a removed builder stays in the buildx copy"; fails=$((fails + 1))
fi
total=$((total + 1))
if [[ -L $dc/config.json || ! -L $dc/contexts ]]; then
	echo "FAIL: state: the docker config links do not follow ~/.docker: $(/bin/ls -lA "$dc" 2>&1)"; fails=$((fails + 1))
fi
# A value that is set wins.
staterun STUB_JOBS="harnesses" BUILDX_CONFIG=/own/buildx KOINON_PNPM_STORE=/own/store BUF_CACHE_DIR=/own/buf DOCKER_CONFIG=/own/docker
expect "state: own values win" 0 '^gate=harnesses .*verdict=pass'
total=$((total + 1))
grep -q 'buildx=/own/buildx pnpm=/own/store buf=/own/buf docker=/own/docker' "$(gate_log harnesses)" || { echo "FAIL: state: own values were replaced"; fails=$((fails + 1)); }
# envtest assets the sandbox cannot fetch: the envtest gates are notrun with the reason, the others run.
staterun STUB_JOBS="escape harnesses" STUB_ASSETS=sandbox
expect "state: envtest assets denied is notrun" 3 '^gate=escape rc=0 .*verdict=notrun reason="envtest assets not found \(sandbox denial\); run make envtest-assets once outside the sandbox"' '^gate=harnesses .*verdict=pass'
# envtest-assets broken for another reason: the gate runs and gives its own verdict (here the stub passes).
staterun STUB_JOBS="escape" STUB_ASSETS=broken
expect "state: other envtest-assets failure is left to the gate" 0 '^gate=escape .*verdict=pass'
# A port lock directory that exists but cannot be written in (a file stands in for the denial).
echo x >"$tmp/lockfile"
staterun STUB_JOBS="escape harnesses" LANE_GATES_ENVTEST_LOCKDIR="$tmp/lockfile"
expect "state: unwritable port locks are notrun" 3 '^gate=escape rc=0 .*verdict=notrun reason="sandbox denial: the envtest port locks in .* are not writable' '^gate=harnesses .*verdict=pass'
# No envtest gate selected: envtest-assets is not called.
staterun STUB_JOBS="harnesses" STUB_ASSETS_MARK="$tmp/assets-called"
expect "state: no envtest gate, no envtest-assets call" 0 '^gate=harnesses .*verdict=pass'
total=$((total + 1))
[[ ! -e $tmp/assets-called ]] || { echo "FAIL: state: envtest-assets ran without an envtest gate"; fails=$((fails + 1)); }
# The control: with an envtest gate it is called.
staterun STUB_JOBS="escape" STUB_ASSETS_MARK="$tmp/assets-called"
total=$((total + 1))
[[ -e $tmp/assets-called ]] || { echo "FAIL: state: envtest-assets did not run for the escape gate"; fails=$((fails + 1)); }

# Usage errors.
out=$(cd "$repo" && bash "$script" --nope 2>&1); rc=$?
expect "unknown flag" 2 'unknown argument'
out=$(cd "$repo" && bash "$script" no-such-ref-xyz 2>&1); rc=$?
expect "unknown base" 2 'unknown base'

if [[ $fails -gt 0 ]]; then
	echo "lane-gates-test: $fails of $total cases failed"
	exit 1
fi
echo "lane-gates-test: $total cases ok"
