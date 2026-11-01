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
# Ginkgo exits non-zero when a suite fails, is empty or was skipped by a focus; plain go test is
# the same for the rest. Pass extra Ginkgo flags with GINKGO_FLAGS, a process count with PROCS.
#
# Per-spec reports (#221): JUnit XML goes to REPORT_DIR (default reports/go, git-ignored), one file
# per Ginkgo suite from `ginkgo --junit-report` and go-test.xml from `gotestsum --junitfile` for the
# plain half, so a local run and CI produce the same files. CI uploads the directory as an artifact.
# The reports only record; nothing reruns a failed spec.
set -euo pipefail
cd "$(dirname "$0")/.."

PARALLEL_SUITES=(
	./internal/agentd
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
find "$report_dir" -maxdepth 1 -name '*.xml' -delete

rc=0
# shellcheck disable=SC2086 # GINKGO_FLAGS is a word list
go tool ginkgo "${procs[@]}" --junit-report=junit.xml --keep-separate-reports --output-dir="$report_dir" --keep-going --randomize-all --fail-on-empty ${GINKGO_FLAGS:-} "${PARALLEL_SUITES[@]}" || rc=$?

rest=()
while IFS= read -r pkg; do
	skip=0
	for s in "${PARALLEL_SUITES[@]}"; do
		[[ $pkg == "github.com/stateracloud/koinon/${s#./}" ]] && skip=1
	done
	[[ $pkg == */test/flavors ]] && skip=1 # needs the flavor images, see make test-flavors
	[[ $skip == 1 ]] || rest+=("$pkg")
done < <(go list ./...)

# Run both halves so one CI run reports every failing package.
go tool gotestsum --junitfile "$report_dir/go-test.xml" --format pkgname -- "${rest[@]}" || rc=$?

exit "$rc"
