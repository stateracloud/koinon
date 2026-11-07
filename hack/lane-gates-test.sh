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
	echo "e2e: informational, always"
	exit 0
fi
g=$2
v=STUB_$g
case ${!v:-ok} in
ok) echo "$g ok 1s" ;;
fail) echo "$g FAIL 1s"; echo "boom"; exit 1 ;;
incomplete) echo "$g INCOMPLETE 1s (skipped: docker)"; echo "INCOMPLETE (missing docker)"; exit 3 ;;
skip) echo "$g skip (pnpm not installed)" ;;
esac
EOF
# Stub ci-changes: prints $STUB_JOBS as name=true.
cat >"$tmp/changes" <<'EOF'
#!/usr/bin/env bash
for j in $STUB_JOBS; do echo "$j=true"; done
echo "docs=false"
EOF
# Stub make: target behaviour from $STUB_MAKE_<target>.
cat >"$tmp/make" <<'EOF'
#!/usr/bin/env bash
case ${STUB_MAKE:-ok} in
ok) echo "made $1" ;;
fail) echo "assertion failed in $1"; exit 1 ;;
nodocker) echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock"; exit 1 ;;
sandbox) echo "open /x: operation not permitted"; exit 1 ;;
esac
EOF
chmod +x "$tmp/preflight" "$tmp/changes" "$tmp/make"

fails=0 total=0
# run <env assignments...>: sets out and rc.
run() {
	out=$(env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make" "$@" bash "$script" HEAD 2>&1)
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
printf '#!/bin/sh\nexit 1\n' >"$tmp/bin-down/docker"
chmod +x "$tmp/bin-up/docker" "$tmp/bin-down/docker"
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

# A failing log that merely says "Sandbox admission policy" is a failure, not a machine problem.
cat >"$tmp/make-sbx" <<'EOF2'
#!/usr/bin/env bash
echo "=== RUN   TestEscape/Sandbox_admission_policy"
echo "--- FAIL: TestEscape (0.1s)"
echo "FAIL	github.com/stateracloud/koinon/internal/sandbox"
exit 1
EOF2
chmod +x "$tmp/make-sbx"
out=$(env LANE_GATES_PREFLIGHT="$tmp/preflight" LANE_GATES_CHANGES="$tmp/changes" LANE_GATES_MAKE="$tmp/make-sbx" STUB_GATES="" STUB_JOBS="escape" bash "$script" HEAD 2>&1); rc=$?
expect "sandbox word is a failure" 1 '^gate=escape rc=1 .*verdict=fail'

# ci-changes turns on go while preflight does not select it: the go gate still runs.
run STUB_GATES="tidy" STUB_JOBS="go"
expect "ci job go maps to gate go" 0 '^gate=go rc=0 .*verdict=pass' '^gate=tidy .*verdict=pass'
run STUB_GATES="go" STUB_JOBS="go"
[[ $(grep -c '^gate=go ' <<<"$out") -eq 1 ]] || { echo "FAIL: go gate must run once"; fails=$((fails + 1)); }

# Usage errors.
out=$(bash "$script" --nope 2>&1); rc=$?
expect "unknown flag" 2 'unknown argument'
out=$(bash "$script" no-such-ref-xyz 2>&1); rc=$?
expect "unknown base" 2 'unknown base'

if [[ $fails -gt 0 ]]; then
	echo "lane-gates-test: $fails of $total cases failed"
	exit 1
fi
echo "lane-gates-test: $total cases ok"
