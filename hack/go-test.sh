#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The Go test run of CI (`make test-go`): the suites listed in PARALLEL_SUITES run with Ginkgo's
# parallel mode (-p, one OS process per core), every other package with plain `go test` (old issue 222).
#
# A suite may be listed only when each parallel process is isolated: pgtest.Start and
# pgtest.StartTemplate run in every process's BeforeSuite, so each process gets databases of its
# own, and the suite binds no fixed port and writes no fixed path. Add a suite here after checking
# that; do not serialise a suite to hide shared state, fix the state.
#
# One Postgres per run (old issue 224): this script starts one server (hack/pgserver) and passes its DSN in
# KOINON_PGTEST_DSN, and every test process of the run, parallel or not, creates its databases there
# as copies of one template migrated once per run (internal/testing/pgtest). The databases are
# still per process (names unique on the server), so the isolation above holds for rows and
# schemas; what processes now share is the server's cluster-wide state, roles and settings, which a
# suite on the default server must leave alone (pgtest's package comment). Durable suites keep a
# container per process. Without Docker no server starts and the suites skip as before (testenv);
# with KOINON_PGTEST_DSN already set, that server is used.
#
# Exit codes alone do not show that a suite ran (old issue 507): a suite that skips as a whole exits 0, and
# under -p a suite whose processes exit before reporting prints "Test Suite Passed" with no report.
# So every Ginkgo suite also writes a JSON report, and hack/testverdict gives each one PASS, FAIL,
# SKIPPED (a missing requirement declared with internal/testing/testenv, e.g. no Docker) or NOT RUN
# (no report, or no spec ran without a declared reason), PASS (partly skipped) when only some specs
# skipped. FAIL and NOT RUN fail the run; a requirement listed in KOINON_REQUIRE (CI sets it) turns
# a skip into a failure.
#
# Exit codes: 0 ok, 1 failed, 3 incomplete: an expected requirement (docker, git, helm by default;
# KOINON_EXPECT overrides, a comma list) was missing, so some suites or specs skipped and the run
# proved less than it should. Requirements that are not expected are opt-in (e2e, openshell,
# envtest, ...) and are only listed.
#
# Usage: hack/go-test.sh [PKG...] (default ./...; the parallel suites among PKG still run with -p).
# Pass extra Ginkgo flags with GINKGO_FLAGS, a process count with PROCS.
#
# Per-spec reports (old issue 221): JUnit XML goes to REPORT_DIR (default reports/go, git-ignored), one file
# per Ginkgo suite (`ginkgo --junit-report` for the parallel ones, `go test -args
# -ginkgo.junit-report` for the rest, one testcase per spec) and go-test.xml from `gotestsum --junitfile` for the packages with only plain tests, so a local run and CI produce the same files. CI uploads the directory as an artifact.
# The Ginkgo JSON reports the verdict reads sit next to them (*_report.json), with the go test
# events of the go test parts (*.gotest.jsonl), where a plain test's testenv.RequireT skip shows.
# The reports only record; nothing reruns a failed spec.
#
# Host guards (issue 551, hack/go-test-host.sh): on macOS the run holds `caffeinate -i -s -w $$`; Docker
# must answer and its clock agree with the host's within 2 s (exit 3 otherwise); a run during which
# `pmset -g log` shows a Sleep ends with exit 3 and `go-test verdict: notrun reason="host slept"`, not
# with test failures; at most KOINON_GO_SLOTS (default 2) runs share a Mac, with PROCS=4 each unless
# PROCS is set; `go test` gets -timeout (GO_TEST_TIMEOUT, default 20m per package);
# ginkgo gets --timeout from GINKGO_TIMEOUT (default 1h), one budget for the whole `ginkgo -p` run.
#
# KOINON_RC_FILE: when set, the final exit status is also written to that file. `make` turns every
# failing recipe into exit 2, so the 3 (incomplete) would not survive `make test-go`; a wrapper
# script reads the file to tell NOT RUN from a failure.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck disable=SC2317,SC2329 # invoked from the exit trap (SC2317 before shellcheck 0.10)
write_rc() { [[ -z ${KOINON_RC_FILE:-} ]] || echo "$1" >"$KOINON_RC_FILE"; }

PARALLEL_SUITES=(
	./internal/agentd
	./internal/runoutcome
	./internal/api
	./internal/durable
	./internal/checkpoint
	./internal/compat
)

if [[ ${1:-} == "--list" ]]; then
	printf '%s\n' "${PARALLEL_SUITES[@]}"
	exit 0
fi
pg_pid=
pg_dir=
# shellcheck disable=SC2317,SC2329 # invoked from the exit trap (SC2317 before shellcheck 0.10)
on_exit() {
	local status=$1
	if [[ -n $pg_pid ]]; then
		kill "$pg_pid" 2>/dev/null || true
		wait "$pg_pid" 2>/dev/null || true
	fi
	[[ -z $pg_dir ]] || rm -rf "$pg_dir"
	write_rc "$status"
}
trap 'on_exit $?' EXIT # after --list: a listing is not a run

# shellcheck source=hack/go-test-host.sh
source "$(dirname "$0")/go-test-host.sh"
host_caffeinate_pid=
host_slot_dir=
# shellcheck disable=SC2317,SC2329 # invoked from the exit trap
host_cleanup() {
	[[ -z $host_caffeinate_pid ]] || kill "$host_caffeinate_pid" 2>/dev/null || true
	host_release_slot
}
# Chain onto the exit trap set above: the guard and the slot go with the run.
# shellcheck disable=SC2154 # rc_ is set inside the trap string
trap 'rc_=$?; host_cleanup; on_exit $rc_' EXIT

host_keep_awake $$

# Four processes per run on a Mac (two runs at once), Ginkgo's default (one per core) elsewhere.
procs=(-p)
if [[ -n ${PROCS:-} ]]; then
	procs=("--procs=$PROCS")
elif host_is_mac; then
	procs=("--procs=4")
fi
# An explicit timeout so a stall fails as one clear package, not at go test's silent 10 m default.
go_timeout=${GO_TEST_TIMEOUT:-20m}
# Ginkgo v2 --timeout is one budget for the whole invocation (all parallel suites), not per package.
ginkgo_timeout=${GINKGO_TIMEOUT:-1h}

report_dir=${REPORT_DIR:-reports/go}
mkdir -p "$report_dir"
# Stale files from an earlier run would read as this run's results.
find "$report_dir" -maxdepth 1 \( -name '*.xml' -o -name '*.json' -o -name '*.jsonl' \) -delete

pkgs=("$@")
((${#pkgs[@]})) || pkgs=(./...)

# Split the packages: the listed parallel suites go to `ginkgo -p`, every other Ginkgo suite keeps
# running through `go test`, which runs packages concurrently (-p defaults to GOMAXPROCS) and gives
# each package its own timeout. Ginkgo's own report flags write <pkgdir>/junit.xml and
# <pkgdir>/report.json per test binary; those files are moved to the report directory afterwards.
# Plain `func Test` packages go to gotestsum.
parallel=()
ginkgo_rest=()
plain=()
# Listed first, not in a process substitution: a failing `go list` (a bad package argument) would
# otherwise leave an empty run that exits 0.
dirs=$(go list -f '{{.Dir}}' "${pkgs[@]}") || { echo "go-test.sh: go list ${pkgs[*]} failed" >&2; exit 1; }
while IFS= read -r dir; do
	[[ -n $dir ]] || continue
	dir=${dir/#"$PWD"/.}
	[[ $# == 0 && $dir == ./test/flavors ]] && continue # needs the flavor images, see make test-flavors
	is_parallel=0
	for s in "${PARALLEL_SUITES[@]}"; do
		[[ $dir == "$s" ]] && is_parallel=1
	done
	if [[ $is_parallel == 1 ]]; then
		parallel+=("$dir")
	elif compgen -G "$dir/*_test.go" >/dev/null && grep -lqs 'RunSpecs(' "$dir"/*_test.go; then
		ginkgo_rest+=("$dir")
	else
		plain+=("$dir")
	fi
done <<<"$dirs"

# A heavy run (Ginkgo suites, the shared Postgres) takes one of the machine's slots and checks Docker
# first; a bad package argument or a plain-only run does neither (issue 551).
if ((${#parallel[@]} + ${#ginkgo_rest[@]})); then
	host_acquire_slot
	host_docker_preflight || exit 3
fi
# The sleep window starts here, with the suites: waiting for a slot can last as long as another heavy
# run, and a Sleep in that wait did not touch these results. Keep-awake is held since the start.
run_start=$(date '+%Y-%m-%d %H:%M:%S')

# One testcontainers session for the whole run (#171). By default every test process is its own
# session lead and shares the run's single Ryuk reaper by name; Ryuk exits 10 s after its last
# client disconnects, and on a cold runner an image pull leaves gaps that long, so a later package
# finds the dying reaper and waits 60 s for it (upstream testcontainers-go#3867, v0.44.0). A fixed
# session ID, exported before hack/pgserver starts, is inherited by pgserver and by every test
# process; pgserver holds a reaper connection until the run ends, so Ryuk stays up the whole run
# and still cleans up after a crashed one. Ryuk stays on. Details: docs/dev/ci.md#go-test-suites.
export TESTCONTAINERS_SESSION_ID=${TESTCONTAINERS_SESSION_ID:-koinon-$$-$(date +%s)-$RANDOM$RANDOM}

# The run's shared Postgres (see the header). The DSN goes through a private temporary directory,
# not the report directory, which CI uploads.
if [[ -z ${KOINON_PGTEST_DSN:-} ]] && ((${#parallel[@]} + ${#ginkgo_rest[@]})); then
	pg_dir=$(mktemp -d "${TMPDIR:-/tmp}/go-test-pg.XXXXXX")
	go build -o "$pg_dir/pgserver" ./hack/pgserver || exit 1
	"$pg_dir/pgserver" -dsn-file "$pg_dir/dsn" &
	pg_pid=$!
	while [[ ! -s $pg_dir/dsn ]] && kill -0 "$pg_pid" 2>/dev/null; do sleep 0.2; done
	if [[ -s $pg_dir/dsn ]]; then
		KOINON_PGTEST_DSN=$(<"$pg_dir/dsn")
		export KOINON_PGTEST_DSN
	else
		wait "$pg_pid" || true
		pg_pid=
		echo "go-test.sh: no shared Postgres for this run (above); each test process starts its own or skips" >&2
	fi
fi

rc=0
failed=()
# Run all parts so one CI run reports every failing package.
if ((${#parallel[@]})); then
	# shellcheck disable=SC2086 # GINKGO_FLAGS is a word list
	go tool ginkgo "${procs[@]}" --timeout="$ginkgo_timeout" --junit-report=junit.xml --json-report=report.json --keep-separate-reports --output-dir="$report_dir" --keep-going --randomize-all --fail-on-empty ${GINKGO_FLAGS:-} "${parallel[@]}" || { prc=$?; rc=$prc; failed+=("ginkgo -p (exit $prc)"); }
fi
if ((${#ginkgo_rest[@]})); then
	for dir in "${ginkgo_rest[@]}"; do rm -f "$dir/junit.xml" "$dir/report.json"; done
	# Through gotestsum for its go test -json events: a plain test in a suite's package that skips
	# through testenv.RequireT is seen only there (the chart contract test without helm).
	go tool gotestsum --jsonfile "$report_dir/ginkgo-rest.gotest.jsonl" --format standard-quiet -- -count=1 -timeout "$go_timeout" "${ginkgo_rest[@]}" -args -ginkgo.junit-report=junit.xml -ginkgo.json-report=report.json -ginkgo.fail-on-empty || { prc=$?; rc=$prc; failed+=("serial Ginkgo suites (exit $prc)"); }
	for dir in "${ginkgo_rest[@]}"; do
		name=${dir#./}
		name=${name//\//_}
		[[ ! -f $dir/junit.xml ]] || mv "$dir/junit.xml" "$report_dir/${name}_junit.xml"
		[[ ! -f $dir/report.json ]] || mv "$dir/report.json" "$report_dir/${name}_report.json"
	done
fi
if ((${#plain[@]})); then
	go tool gotestsum --junitfile "$report_dir/go-test.xml" --jsonfile "$report_dir/plain.gotest.jsonl" --format pkgname -- -timeout "$go_timeout" "${plain[@]}" || { prc=$?; rc=$prc; failed+=("plain packages (exit $prc)"); }
fi

# The verdict over every Ginkgo suite of this run; it fails the run for a suite that did not run.
suites=("${parallel[@]}" "${ginkgo_rest[@]}")
events=()
pflags=()
# ginkgo -p emits no go test events: the verdict fails a plain RequireT test in these packages.
for d in ${parallel[@]+"${parallel[@]}"}; do pflags+=(-parallel "$d"); done
for f in "$report_dir"/*.gotest.jsonl; do [[ -f $f ]] && events+=(-gotest-json "$f"); done
if ((${#suites[@]} + ${#events[@]})); then
	# Built, not `go run`: go run reports any non-zero exit of the program as 1, hiding the 3.
	go build -o "$report_dir/testverdict.bin" ./hack/testverdict || exit 1
	vrc=0
	"$report_dir/testverdict.bin" -reports "$report_dir" -name go-test -expect "${KOINON_EXPECT:-docker,git,helm}" ${events[@]+"${events[@]}"} ${pflags[@]+"${pflags[@]}"} ${suites[@]+"${suites[@]}"} || vrc=$?
	rm -f "$report_dir/testverdict.bin"
	if [[ $vrc == 3 && $rc == 0 ]]; then
		rc=3
	elif [[ $vrc != 0 && $vrc != 3 ]]; then
		rc=1
	fi
fi

# A host that slept makes the run invalid, whatever the suites reported.
if host_slept_between "$run_start" "$(date '+%Y-%m-%d %H:%M:%S')"; then
	echo "go-test.sh: the host slept during this run (pmset -g log); the results mean nothing" >&2
	echo 'go-test verdict: notrun reason="host slept"'
	rc=3
	failed=()
fi

# The verdict above counts Ginkgo specs only: a plain test that failed shows there as "0 failed".
# Name the failing part, so "go-test verdict: ok" followed by a non-zero exit is not a mystery (old issue 512).
if ((${#failed[@]})); then
	echo "go-test.sh: failing part(s): ${failed[*]}; the verdict line above counts Ginkgo specs only, see the '=== FAIL' lines of the go test output" >&2
fi
exit "$rc"
