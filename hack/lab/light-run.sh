#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs one light-lane target inside the home lab's light workflow (hack/lab/light.workflow.json), in
# the checked-out commit: the make target, its output in $OUT/driver.log and a result line in
# $OUT/result.txt. Only the targets below run; they need Docker (the pod's dind) at most, no kind.
#
#   hack/lab/light-run.sh <target>
set -euo pipefail

target=${1:?usage: light-run.sh <target>}
out=${OUT:?OUT must name the results directory}
case $target in
test-go | test-admission | test-chart-smoke | chart-check | docs-check) ;;
*) echo "light lane: $target is not a light target (test-go, test-admission, test-chart-smoke, chart-check, docs-check)" >&2; exit 2 ;;
esac

mkdir -p "$out"
echo "== $(date -u +%H:%M:%SZ) make $target"
start=$(date +%s)
set +e
make "$target" 2>&1 | tee "$out/driver.log"
rc=${PIPESTATUS[0]}
set -e
echo "target=$target ref=$(git rev-parse HEAD) rc=$rc seconds=$(($(date +%s) - start))" | tee "$out/result.txt"
exit "$rc"
