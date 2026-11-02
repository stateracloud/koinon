#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs one gate set inside a lab's koinon-gate workflow, in the checked-out commit: a fixed command
# per gate set, its output in $OUT/driver.log, the per-spec reports in $OUT/reports and a result line in
# $OUT/result.txt. The successor of hack/lab/light-run.sh (ADR 0042 stage 2, #512). The gate set is
# an enum; anything else exits 2, so the workflow's gate-set parameter is never a free-form command.
#
#   hack/lab/gate-run.sh <gate-set>
#
#   go-test         make test-go                                    (every Go package)
#   agentd-ginkgo   make test-go-suite SUITE=./internal/agentd      (one parallel suite, ginkgo -p)
#   test-admission, test-chart-smoke, chart-check, docs-check       (the former light targets, make <set>)
#
# result.txt: `target=<set> ref=<sha> rc=<n> seconds=<n> verdict=pass|fail|notrun`.
#   pass    the command exited 0.
#   fail    it exited non-zero (rc is its status; 1 when it reported none).
#   notrun  the run proved less than it should (#507): hack/go-test.sh exited 3, an expected
#           requirement such as docker was missing, so suites or specs skipped. Only the Go gates can
#           say so. `make` turns a recipe's 3 into its own 2, so go-test.sh also writes its status to
#           KOINON_RC_FILE and that file is what is read here.
# Exit status: 0 for pass, the command's status for fail, 3 for notrun (the #507 contract).
#
# Environment: OUT (required), PROCS (parallel Ginkgo processes; default 4: ginkgo -p sizes itself
# from the node's cores, not from the pod's cgroup limit, and each process starts a Postgres
# container in the pod's dind), GOFLAGS (-timeout=25m is added: a run shares the node's cores).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

set_name=${1:?usage: gate-run.sh <gate-set>}
out=${OUT:?OUT must name the results directory}
go_gate=0
case $set_name in
go-test) cmd=(make test-go) go_gate=1 ;;
agentd-ginkgo) cmd=(make test-go-suite SUITE=./internal/agentd) go_gate=1 ;;
test-admission | test-chart-smoke | chart-check | docs-check) cmd=(make "$set_name") ;;
*)
	echo "gate-run: $set_name is not a gate set (go-test, agentd-ginkgo, test-admission, test-chart-smoke, chart-check, docs-check)" >&2
	exit 2 ;;
esac

mkdir -p "$out"
rc_file=$out/go-test.rc
rm -f "$rc_file"
if [[ $go_gate == 1 ]]; then
	export PROCS=${PROCS:-4} REPORT_DIR=$out/reports KOINON_RC_FILE=$rc_file
fi
echo "== $(date -u +%H:%M:%SZ) ${cmd[*]}"
start=$(date +%s)
set +e
GOFLAGS="${GOFLAGS:+$GOFLAGS }-timeout=25m" "${cmd[@]}" 2>&1 | tee "$out/driver.log"
rc=${PIPESTATUS[0]}
set -e
if [[ $go_gate == 1 && -s $rc_file ]]; then rc=$(cat "$rc_file"); fi
case $rc in
0) verdict=pass ;;
3) if [[ $go_gate == 1 ]]; then verdict=notrun; else verdict=fail; fi ;;
*) verdict=fail ;;
esac
echo "target=$set_name ref=$(git rev-parse HEAD) rc=$rc seconds=$(($(date +%s) - start)) verdict=$verdict" | tee "$out/result.txt"
exit "$rc"
