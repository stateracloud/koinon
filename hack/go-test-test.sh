#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# hack/go-test.sh runs its PARALLEL_SUITES with Ginkgo -p (old issue 222). Fails when a listed suite is not
# a Ginkgo suite, when one starts Postgres outside a BeforeSuite (the container would then be shared
# or missing in the other processes), or when one binds a fixed local port.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

# Offline: no machine-wide slot shared with other runs, no real Docker ping.
KOINON_GO_SLOTS_DIR=$(mktemp -d "${TMPDIR:-/tmp}/go-test-slots.XXXXXX")
export KOINON_GO_SLOTS_DIR KOINON_DOCKER=koinon-no-such-docker

suites=$(hack/go-test.sh --list)
[[ -n $suites ]] || { echo "FAIL: hack/go-test.sh lists no parallel suites"; exit 1; }
for s in $suites; do
	[[ -d $s ]] || { echo "FAIL: $s is not a directory"; exit 1; }
	grep -lq 'RunSpecs(' "$s"/*_suite_test.go || { echo "FAIL: $s has no Ginkgo suite"; exit 1; }
	# A fixed port makes two processes of one suite collide.
	if grep -nE 'net\.Listen\("tcp", *"[^"]*:[1-9][0-9]*"|ListenAndServe\("[^"]*:[1-9][0-9]*"' "$s"/*_test.go; then
		echo "FAIL: $s binds a fixed port; use :0 so parallel processes do not collide"; exit 1
	fi
	# Package-level Postgres start (outside any node) would run once per process at init, before Ginkgo.
	if grep -nE '^(var|func init).*pgtest\.' "$s"/*_test.go; then
		echo "FAIL: $s starts Postgres at package level; do it in BeforeSuite"; exit 1
	fi
done
echo "ok: $(echo "$suites" | tr '\n' ' ')"

# #171: one testcontainers session per run, exported before pgserver starts and Ryuk left on.
sid=$(grep -n '^export TESTCONTAINERS_SESSION_ID=' hack/go-test.sh | head -1 | cut -d: -f1)
pgs=$(grep -n 'pgserver" -dsn-file' hack/go-test.sh | head -1 | cut -d: -f1)
[[ -n $sid ]] || { echo "FAIL: go-test.sh does not export TESTCONTAINERS_SESSION_ID"; exit 1; }
[[ -n $pgs && $sid -lt $pgs ]] || { echo "FAIL: TESTCONTAINERS_SESSION_ID must be exported before pgserver starts"; exit 1; }
! grep -q 'TESTCONTAINERS_RYUK_DISABLED' hack/go-test.sh .github/workflows/ci.yml || { echo "FAIL: Ryuk must stay on (#171)"; exit 1; }
echo "ok: one testcontainers session per run, exported before pgserver"

# Per-spec reports (old issue 221): the script writes them and CI uploads them even when tests fail.
grep -q -- '--junit-report' hack/go-test.sh || { echo "FAIL: go-test.sh writes no Ginkgo JUnit report"; exit 1; }
grep -q -- 'gotestsum --junitfile' hack/go-test.sh || { echo "FAIL: go-test.sh writes no gotestsum JUnit file"; exit 1; }
wf=.github/workflows/ci.yml
for job in go-test go-race-serial go-race-parallel; do
	body=$(sed -n "/^  $job:\$/,/^  [a-z-]*:\$/p" "$wf")
	grep -q 'upload-artifact@' <<<"$body" || { echo "FAIL: $job does not upload its reports"; exit 1; }
	grep -q 'if: always()' <<<"$body" || { echo "FAIL: $job uploads reports only on success (needs if: always())"; exit 1; }
done
echo "ok: per-spec reports written and uploaded"

# Every Ginkgo suite gets per-spec rows: the serial ones go through go test with -ginkgo.junit-report (keeping package parallelism).
grep -q 'ginkgo_rest' hack/go-test.sh || { echo "FAIL: go-test.sh does not report the serial Ginkgo suites"; exit 1; }
grep -q -- '-ginkgo.junit-report' hack/go-test.sh || { echo "FAIL: serial Ginkgo suites do not write -ginkgo.junit-report"; exit 1; }
! grep -q -- '--procs=1' hack/go-test.sh || { echo "FAIL: serial suites run one at a time through ginkgo --procs=1 (loses package parallelism)"; exit 1; }
grep -q 'ginkgo -race.*--junit-report' .github/workflows/ci.yml || { echo "FAIL: go-race does not use ginkgo for its per-spec report"; exit 1; }
echo "ok: serial Ginkgo suites and go-race report per spec"

# Issue 44: the race detector covers the concurrent packages, and the parallel ones run with -p only
# when hack/go-test.sh lists them as parallel suites.
race=$(sed -n '/^  go-race-serial:$/,/^  go:$/p' "$wf")
for pkg in reconciler ledger agentd durable; do
	grep -q "ginkgo -race.* ./internal/$pkg/" <<<"$race" || { echo "FAIL: go-race does not run ./internal/$pkg under -race"; exit 1; }
done
for pkg in agentd durable; do
	grep -q "ginkgo -race -p .*./internal/$pkg/" <<<"$race" || { echo "FAIL: go-race runs $pkg without -p"; exit 1; }
	grep -q "^	./internal/$pkg\$" hack/go-test.sh || { echo "FAIL: go-race runs $pkg with -p but go-test.sh does not list it as parallel"; exit 1; }
done
grep -c -- 'ginkgo -race.* --keep-going ' <<<"$race" | grep -qx 2 || { echo "FAIL: a go-race ginkgo step lacks --keep-going (a failing first suite skips the second)"; exit 1; }
grep -q 'uses: ./.github/actions/agentgateway-test-image' <<<"$race" || { echo "FAIL: go-race runs the ledger suite without pulling the agentgateway test image"; exit 1; }
grep -q 'packages: read' <<<"$race" || { echo "FAIL: go-race cannot read the GHCR agentgateway copy (packages: read)"; exit 1; }
echo "ok: go-race covers reconciler, ledger, agentd and durable"

# One EXIT trap removes every fixture dir and file this script makes. Each path is registered in
# fixtures right after its mktemp, so an early exit (set -u or a failed check) still removes it.
fixtures=()
fixtures+=("$KOINON_GO_SLOTS_DIR")
cleanup_fixtures() {
	local p
	for p in "${fixtures[@]}"; do
		[[ -n $p ]] && rm -rf -- "$p"
	done
	return 0
}
trap cleanup_fixtures EXIT

# Behaviour check, opt-in because it runs the suites (needs Docker): a Ginkgo suite outside
# PARALLEL_SUITES must leave a report with more than one testcase.
if [[ ${GO_TEST_TEST_RUN:-} == 1 ]]; then
	d=$(mktemp -d "${TMPDIR:-/tmp}/go-test-test.XXXXXX")
	fixtures+=("$d")
	REPORT_DIR=$d hack/go-test.sh >/dev/null 2>&1 || true
	f=$(find "$d" -name 'junit_*reconciler*.xml' | head -1)
	[[ -n $f && $(grep -c '<testcase' "$f") -gt 1 ]] || { echo "FAIL: reconciler report has no per-spec testcases"; exit 1; }
	echo "ok: serial Ginkgo suite report has per-spec testcases"
fi

# old issue 507: a Ginkgo suite where no spec ran is not a pass. ./internal/runoutcome has Postgres specs
# only, so without Docker every one of them skips through testenv.Require. A dead DOCKER_HOST does
# not simulate that (testcontainers falls back to the default socket), KOINON_TESTENV_UNAVAILABLE does.
d=$(mktemp -d "${TMPDIR:-/tmp}/go-test-test.XXXXXX")
fixtures+=("$d")
rc=0
out=$(KOINON_TESTENV_UNAVAILABLE=docker REPORT_DIR=$d hack/go-test.sh ./internal/runoutcome 2>&1) || rc=$?
[[ $rc == 3 ]] || { echo "$out" | tail -20; echo "FAIL: a suite that skips for a missing expected requirement must exit 3 (incomplete), got $rc"; exit 1; }
grep -q 'SKIPPED, NOT PASSED' <<<"$out" || { echo "$out" | tail -20; echo "FAIL: a suite skipped for a missing requirement is not reported as SKIPPED, NOT PASSED"; exit 1; }
d=$(mktemp -d "${TMPDIR:-/tmp}/go-test-test.XXXXXX")
fixtures+=("$d")
if KOINON_REQUIRE=docker KOINON_TESTENV_UNAVAILABLE=docker REPORT_DIR=$d hack/go-test.sh ./internal/runoutcome >/dev/null 2>&1; then
	echo "FAIL: KOINON_REQUIRE=docker must fail a suite whose Docker is missing"; exit 1
fi
d=$(mktemp -d "${TMPDIR:-/tmp}/go-test-test.XXXXXX")
fixtures+=("$d")
if out=$(go run ./hack/testverdict -reports "$d" ./internal/runoutcome 2>&1); then
	echo "FAIL: testverdict passed a suite that left no report"; exit 1
fi
grep -q 'NOT RUN' <<<"$out" || { echo "$out"; echo "FAIL: testverdict does not report a suite without a report as NOT RUN"; exit 1; }
echo "ok: skipped suites are SKIPPED, KOINON_REQUIRE fails them, a missing report is NOT RUN"

# A package argument go list rejects must fail the run, not leave an empty run that exits 0.
d=$(mktemp -d "${TMPDIR:-/tmp}/go-test-test.XXXXXX")
fixtures+=("$d")
if REPORT_DIR=$d hack/go-test.sh ./internal/no-such-package >/dev/null 2>&1; then
	echo "FAIL: go-test.sh passed a package that go list rejects"; exit 1
fi
echo "ok: a failing go list fails the run"

# A plain test that skips through testenv.RequireT is seen through go test's events: the chart
# contract test (a plain test in the chartsmoke suite's package) without helm makes the run
# incomplete, even though the suite's own specs skip for the opt-in envtest.
d=$(mktemp -d "${TMPDIR:-/tmp}/go-test-test.XXXXXX")
fixtures+=("$d")
rc=0
out=$(env -u KUBEBUILDER_ASSETS HELM=koinon-no-such-helm REPORT_DIR="$d" hack/go-test.sh ./test/chartsmoke 2>&1) || rc=$?
[[ $rc == 3 ]] || { echo "$out" | tail -20; echo "FAIL: chart contract test without helm: exit $rc, want 3 (incomplete)"; exit 1; }
grep -q 'INCOMPLETE (missing helm)' <<<"$out" || { echo "$out" | tail -20; echo "FAIL: the verdict does not name helm"; exit 1; }
echo "ok: a plain test skipped for an expected requirement makes the run incomplete"

# make test-go-suite SUITE=<name> (old issue 512): one of --list, nothing free-form. The refusals do not start
# any test, so they run anywhere.
for bad in "" ./internal/nope "./internal/agentd ./internal/api" "../etc"; do
	rc=0
	SUITE="$bad" make -s test-go-suite SUITE="$bad" >/dev/null 2>&1 || rc=$?
	# make reports a failed recipe as 2, which is also the usage code here.
	[[ $rc == 2 ]] || { echo "FAIL: make test-go-suite SUITE='$bad' must exit 2, got $rc"; exit 1; }
done
echo "ok: test-go-suite refuses a suite that hack/go-test.sh --list does not name"
rc_file=$(mktemp "${TMPDIR:-/tmp}/go-test-test.XXXXXX")
fixtures+=("$rc_file")
rd=$(mktemp -d "${TMPDIR:-/tmp}/go-test-test.XXXXXX")
fixtures+=("$rd")
KOINON_RC_FILE=$rc_file REPORT_DIR=$rd hack/go-test.sh ./no/such/pkg >/dev/null 2>&1 || true
[[ $(cat "$rc_file") == 1 ]] || { echo "FAIL: KOINON_RC_FILE must hold go-test.sh's exit status (1 for a bad package)"; exit 1; }
echo "ok: KOINON_RC_FILE carries the exit status past make"

# old issue 512: a plain test that fails must fail the run and be named, even though the Ginkgo verdict over
# the same run says "ok ... 0 failed" (the first external gate run: a flaky plain test, exit 1, no hint).
fx=$(mktemp -d "$root/internal/zzfailfixture.XXXXXX")
fixtures+=("$fx")
printf '// SPDX-License-Identifier: Apache-2.0\npackage zzfail\n\nimport "testing"\n\nfunc TestFails(t *testing.T) { t.Fatal("deliberate") }\n' >"$fx/fail_test.go"
rc=0
rd2=$(mktemp -d "${TMPDIR:-/tmp}/go-test-test.XXXXXX")
fixtures+=("$rd2")
out=$(REPORT_DIR=$rd2 hack/go-test.sh "./${fx#"$root"/}" 2>&1) || rc=$?
[[ $rc == 1 ]] || { echo "$out" | tail -20; echo "FAIL: a failing plain test must exit 1, got $rc"; exit 1; }
grep -q 'failing part(s): plain packages (exit 1)' <<<"$out" || { echo "$out" | tail -20; echo "FAIL: the failing part is not named"; exit 1; }
grep -q 'go-test verdict: FAILED' <<<"$out" || { echo "$out" | tail -20; echo "FAIL: the verdict line must say FAILED for a failed plain test, not ok"; exit 1; }
echo "ok: a failing plain test fails the run, names the part and fails the verdict"

# Plain RequireT tests in a ginkgo -p suite (#106): go-test.sh hands every parallel suite to the verdict as
# -parallel, and the verdict fails a suite directory whose plain test reaches testenv.RequireT (here through a
# helper), telling the developer how to fix it.
grep -qF -e 'pflags+=(-parallel ' hack/go-test.sh || { echo "FAIL: go-test.sh does not pass its parallel suites to testverdict as -parallel"; exit 1; }
pf=$(mktemp -d "${TMPDIR:-/tmp}/go-test-test.XXXXXX")
fixtures+=("$pf")
printf '// SPDX-License-Identifier: Apache-2.0\npackage zzparallel\n\nimport "testing"\n\nfunc needHelm(t *testing.T) { testenv.RequireT(t, testenv.Helm) }\n\nfunc TestNeedsHelm(t *testing.T) { needHelm(t) }\n' >"$pf/x_test.go"
rc=0
rd3=$(mktemp -d "${TMPDIR:-/tmp}/go-test-test.XXXXXX")
fixtures+=("$rd3")
out=$(go run ./hack/testverdict -reports "$rd3" -parallel "$pf" "$pf" 2>&1) || rc=$?
[[ $rc != 0 ]] || { echo "$out"; echo "FAIL: testverdict passed a plain RequireT test in a -parallel suite"; exit 1; }
grep -q 'TestNeedsHelm' <<<"$out" || { echo "$out"; echo "FAIL: the verdict does not name the plain RequireT test"; exit 1; }
grep -q 'take the package off PARALLEL_SUITES' <<<"$out" || { echo "$out"; echo "FAIL: the verdict does not say how to fix it"; exit 1; }
rm -rf "$pf" "$rd3"
echo "ok: a plain RequireT test in a -parallel suite fails the verdict and says how to fix it"
