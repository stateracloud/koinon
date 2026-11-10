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
v=STUB_$g
case ${!v:-ok} in
ok) echo "$g ok 1s" ;;
fail) echo "$g FAIL 1s"; echo "boom"; exit 1 ;;
incomplete) echo "$g INCOMPLETE 1s (skipped: docker)"; echo "INCOMPLETE (missing docker)"; exit 3 ;;
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
# A stub docker on PATH decides whether the Docker-needing jobs (chartsmoke, flavors) can run.
mkdir "$tmp/bin-up" "$tmp/bin-down"
printf '#!/bin/sh\nexit 0\n' >"$tmp/bin-up/docker"
printf '#!/bin/sh\nexit 0\n' >"$tmp/bin-up/helm"
printf '#!/bin/sh\nexit 0\n' >"$tmp/bin-up/go"
printf '#!/bin/sh\nexit 1\n' >"$tmp/bin-down/docker"
chmod +x "$tmp/bin-up/helm" "$tmp/bin-up/go" "$tmp/bin-up/docker" "$tmp/bin-down/docker"
run PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="chartsmoke flavors escape"
expect "jobs pass" 0 '^gate=chartsmoke .*verdict=pass' '^gate=flavors .*verdict=pass' '^gate=escape .*verdict=pass'
run PATH="$tmp/bin-down:$PATH" STUB_GATES="" STUB_JOBS="chartsmoke escape"
expect "no docker daemon" 3 '^gate=chartsmoke rc=0 .*verdict=notrun reason="docker unavailable"' '^gate=escape .*verdict=pass'
run STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=fail
expect "job fail" 1 '^gate=escape rc=1 .*verdict=fail'
run STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=nodocker
expect "job no docker" 3 '^gate=escape rc=1 .*verdict=notrun reason="docker unavailable"'
run STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=sandbox
expect "job sandbox" 3 '^gate=escape rc=1 .*verdict=notrun reason="sandbox denial"'
run STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=dockerperm
expect "job docker socket denied" 3 '^gate=escape rc=1 .*verdict=notrun reason="docker unavailable"'
run STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=network
expect "job network denied" 3 '^gate=escape rc=1 .*verdict=notrun reason="network denied"'
run STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=netallow
expect "job allowed_domains denial" 3 '^gate=escape rc=1 .*verdict=notrun reason="network denied"'
run STUB_GATES="" STUB_JOBS="escape" STUB_MAKE=networkfail
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
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make-sbx" STUB_GATES="" STUB_JOBS="escape" bash "$script" "$nonempty_base" 2>&1); rc=$?
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
out=$(cd "$repo" && env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make-mixed" STUB_GATES="" STUB_JOBS="escape" bash "$script" "$nonempty_base" 2>&1); rc=$?
expect "failure beats docker text" 1 '^gate=escape rc=1 .*verdict=fail'

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
run LANE_GATES_MISSING_TOOLS="go" PATH="$tmp/bin-up:$PATH" STUB_GATES="" STUB_JOBS="chartsmoke flavors escape"
expect "go missing" 3 '^gate=chartsmoke .*verdict=notrun reason="go not installed"' '^gate=flavors .*verdict=notrun reason="go not installed"' '^gate=escape .*verdict=notrun reason="go not installed"'

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
# Hooks print a warning.
run STUB_GATES="go"
expect "hook warning" 0 'warning: test hook LANE_GATES_PREFLIGHT is set'

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
