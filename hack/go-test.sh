#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The Go test run of CI (`make test-go`): the suites listed in PARALLEL_SUITES run with Ginkgo's
# parallel mode (-p, one OS process per core), every other package with plain `go test` (#222).
#
# A suite may be listed only when each parallel process is isolated: pgtest.Start and
# pgtest.StartTemplate run in every process's BeforeSuite, so each process gets its own Postgres
# container, and the suite binds no fixed port and writes no fixed path. Add a suite here after
# checking that; do not serialise a suite to hide shared state, fix the state.
#
# Exit codes alone do not show that a suite ran (#507): a suite that skips as a whole exits 0, and
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
# Per-spec reports (#221): JUnit XML goes to REPORT_DIR (default reports/go, git-ignored), one file
# per Ginkgo suite (`ginkgo --junit-report` for the parallel ones, `go test -args
# -ginkgo.junit-report` for the rest, one testcase per spec) and go-test.xml from `gotestsum --junitfile` for the packages with only plain tests, so a local run and CI produce the same files. CI uploads the directory as an artifact.
# The Ginkgo JSON reports the verdict reads sit next to them (*_report.json).
# The reports only record; nothing reruns a failed spec.
set -euo pipefail
cd "$(dirname "$0")/.."

PARALLEL_SUITES=(
	./internal/agentd
	./internal/runoutcome
	./internal/api
	./internal/durable
	./internal/checkpoint
)

if [[ ${1:-} == "--list" ]]; then
	printf '%s\n' "${PARALLEL_SUITES[@]}"
	exit 0
fi

procs=(-p)
[[ -z ${PROCS:-} ]] || procs=("--procs=$PROCS")

report_dir=${REPORT_DIR:-reports/go}
mkdir -p "$report_dir"
# Stale files from an earlier run would read as this run's results.
find "$report_dir" -maxdepth 1 \( -name '*.xml' -o -name '*.json' \) -delete

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
while IFS= read -r dir; do
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
done < <(go list -f '{{.Dir}}' "${pkgs[@]}" | sed "s|^$PWD|.|")

rc=0
# Run all parts so one CI run reports every failing package.
if ((${#parallel[@]})); then
	# shellcheck disable=SC2086 # GINKGO_FLAGS is a word list
	go tool ginkgo "${procs[@]}" --junit-report=junit.xml --json-report=report.json --keep-separate-reports --output-dir="$report_dir" --keep-going --randomize-all --fail-on-empty ${GINKGO_FLAGS:-} "${parallel[@]}" || rc=$?
fi
if ((${#ginkgo_rest[@]})); then
	for dir in "${ginkgo_rest[@]}"; do rm -f "$dir/junit.xml" "$dir/report.json"; done
	go test -count=1 "${ginkgo_rest[@]}" -args -ginkgo.junit-report=junit.xml -ginkgo.json-report=report.json -ginkgo.fail-on-empty || rc=$?
	for dir in "${ginkgo_rest[@]}"; do
		name=${dir#./}
		name=${name//\//_}
		[[ ! -f $dir/junit.xml ]] || mv "$dir/junit.xml" "$report_dir/${name}_junit.xml"
		[[ ! -f $dir/report.json ]] || mv "$dir/report.json" "$report_dir/${name}_report.json"
	done
fi
if ((${#plain[@]})); then
	go tool gotestsum --junitfile "$report_dir/go-test.xml" --format pkgname -- "${plain[@]}" || rc=$?
fi

# The verdict over every Ginkgo suite of this run; it fails the run for a suite that did not run.
suites=(${parallel[@]+"${parallel[@]}"} ${ginkgo_rest[@]+"${ginkgo_rest[@]}"}) # bash 3.2: empty arrays under set -u
if ((${#suites[@]})); then
	# Built, not `go run`: go run reports any non-zero exit of the program as 1, hiding the 3.
	go build -o "$report_dir/testverdict.bin" ./hack/testverdict || exit 1
	vrc=0
	"$report_dir/testverdict.bin" -reports "$report_dir" -name go-test -expect "${KOINON_EXPECT:-docker,git,helm}" "${suites[@]}" || vrc=$?
	rm -f "$report_dir/testverdict.bin"
	if [[ $vrc == 3 && $rc == 0 ]]; then
		rc=3
	elif [[ $vrc != 0 && $vrc != 3 ]]; then
		rc=1
	fi
fi

exit "$rc"
