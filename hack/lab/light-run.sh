#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs one light-lane target inside a lab's light workflow (KOINON_E2E_LIGHT_MANIFEST, see
# hack/e2e-argo.sh), in the checked-out commit: the make target, its output in $OUT/driver.log and a result line in
# $OUT/result.txt. Only the targets below run; they need Docker (the pod's dind) at most, no kind.
#
#   hack/lab/light-run.sh <target>

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

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
# A run shares the e2e node's cores with others; go test's default 10-minute per-package timeout
# fails slow-but-passing packages under contention (agentd, api in the 2026-10-31 load test).
GOFLAGS="${GOFLAGS:+$GOFLAGS }-timeout=25m" make "$target" 2>&1 | tee "$out/driver.log"
rc=${PIPESTATUS[0]}
set -e
echo "target=$target ref=$(git rev-parse HEAD) rc=$rc seconds=$(($(date +%s) - start))" | tee "$out/result.txt"
exit "$rc"
