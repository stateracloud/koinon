#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The restore drill (ADR 0109, issue #514): builds the cluster (kind), loads the images, runs the
# Ginkgo suite in this directory and decides the exit code. The suite seeds a source install, backs
# it up with the guide's commands, declares it lost and restores into a fresh namespace.
#
# Exit codes: 0 every check passed or failed only on a declared known gap (label known-gap, the
# message "known gap: #N"); 1 a check failed that is not a known gap, or the suite did not finish.
#
#   KEEP=1                   leave the cluster and the namespaces (inspect, then test/drill/sweep.sh)
#   SKIP_UP=1 SKIP_BUILD=1   the cluster and the images are there already
#   CLUSTER=koinon-e1        the kind cluster (kind topology only)
#   DRILL_TOPOLOGY=<file>    another topology (the home lab: the file lives outside the repository).
#                            It needs an explicit KUBECONFIG, and run.sh makes no cluster and loads
#                            no images then.
#   DRILL_RUN=<id>           the run ID (default: random); every object carries koinon.dev/drill-run=<id>
#   OUT=<dir>                logs, junit.xml, report.json and drill-result.json
#
# Only the kube context named by the topology is used. KUBECONFIG is exported explicitly, never read
# from the ambient shell on kind: run.sh writes the kind cluster's own kubeconfig into $OUT.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
here="$root/test/drill"
CLUSTER=${CLUSTER:-koinon-e1}
KEEP=${KEEP:-0}
RUN=${DRILL_RUN:-$(LC_ALL=C tr -dc 'a-f0-9' </dev/urandom | head -c 6 || true)}
OUT=${OUT:-$(mktemp -d "${TMPDIR:-/tmp}/drill.XXXXXX")}
mkdir -p "$OUT/bin"
status=1

if [[ -n ${DRILL_TOPOLOGY:-} ]]; then
  topology=$DRILL_TOPOLOGY
  SKIP_UP=1
  SKIP_BUILD=1
  [[ -n ${KUBECONFIG:-} && $KUBECONFIG != /dev/null ]] || { echo "DRILL_TOPOLOGY needs an explicit KUBECONFIG" >&2; exit 2; }
  kubeconfig=$KUBECONFIG
  created_cluster=0
else
  topology=$here/topology/kind.yaml
  SKIP_UP=${SKIP_UP:-0}
  SKIP_BUILD=${SKIP_BUILD:-0}
  kubeconfig=$OUT/kubeconfig
  created_cluster=1
fi

cleanup() {
  local rc=$?
  if [[ -s $kubeconfig && $KEEP != 1 ]]; then
    KUBECONFIG=$kubeconfig "$here/sweep.sh" "$RUN" 2>&1 | tee "$OUT/sweep.log" || true
  fi
  if [[ -s $kubeconfig && $status -ne 0 ]]; then
    {
      kubectl --kubeconfig "$kubeconfig" get pods -A -o wide
      kubectl --kubeconfig "$kubeconfig" get events -A --sort-by=.lastTimestamp
    } >"$OUT/cluster-state.txt" 2>&1 || true
  fi
  if [[ $created_cluster == 1 && $KEEP != 1 && ${DRILL_KEEP_CLUSTER:-0} != 1 ]]; then CLUSTER="$CLUSTER" "$root/test/e2e/kind/down.sh" || true; fi
  echo "logs, reports and drill-result.json in $OUT"
  exit "$rc"
}
trap cleanup EXIT

step() { echo "== $(date -u +%H:%M:%SZ) $*"; }

if [[ $SKIP_UP != 1 ]]; then
  step "prerequisites (kind/up.sh)"
  CLUSTER="$CLUSTER" "$root/test/e2e/kind/up.sh" 2>&1 | tee "$OUT/up.log"
fi
if [[ $created_cluster == 1 ]]; then
  kind get kubeconfig --name "$CLUSTER" >"$kubeconfig"
  chmod 600 "$kubeconfig"
fi
if [[ $SKIP_BUILD != 1 ]]; then
  step "images (kind/images.sh)"
  CLUSTER="$CLUSTER" "$root/test/e2e/kind/images.sh" 2>&1 | tee "$OUT/images.log"
fi

step "build the drill suite (go test -c)"
go test -c -o "$OUT/bin/drill.test" "$root/test/drill/"

step "run the drill (run $RUN)"
set +e
(
  cd "$here"
  KOINON_REQUIRE=drill \
  KOINON_DRILL_KUBECONFIG="$kubeconfig" \
  KOINON_DRILL_TOPOLOGY="$topology" \
  KOINON_DRILL_REPO="$root" \
  KOINON_DRILL_OUT="$OUT" \
  KOINON_DRILL_RUN="$RUN" \
  KOINON_DRILL_KEEP="$KEEP" \
  KOINON_DRILL_COMMIT="$(git -C "$root" rev-parse HEAD 2>/dev/null || true)" \
    "$OUT/bin/drill.test" -test.timeout 90m -ginkgo.v -ginkgo.timeout=85m \
      -ginkgo.junit-report="$OUT/junit.xml" -ginkgo.json-report="$OUT/report.json"
) 2>&1 | tee "$OUT/drill.log"
suite_rc=${PIPESTATUS[0]}
set -e

[[ -s $OUT/report.json ]] || { echo "the suite wrote no report.json (exit $suite_rc)" >&2; exit 1; }
[[ -s $OUT/drill-result.json ]] || { echo "the suite wrote no drill-result.json (exit $suite_rc)" >&2; exit 1; }

# The suite exits non-zero when a known-gap spec fails, and that is the point of a known gap: it is red
# in the report and does not make the job red. The exit code follows drill-result.json, whose verdict
# counts a known gap only for a spec with the known-gap label AND a "known gap: #N" message.
unexpected=$(jq '[.[].SpecReports[]
  | select(.LeafNodeType == "It")
  | select(.State == "failed" or .State == "panicked" or .State == "timedout" or .State == "interrupted" or .State == "aborted")
  | select(((.LeafNodeLabels // []) | index("known-gap")) | not)] | length' "$OUT/report.json")
ran=$(jq '[.[].SpecReports[] | select(.LeafNodeType == "It" and .State == "passed")] | length' "$OUT/report.json")
gaps=$(jq -r '[.knownGaps[]? | .issue + " " + .name] | join(", ")' "$OUT/drill-result.json")
echo
echo "drill: $ran spec(s) passed, $unexpected failed outside the known gaps; known gaps failing: ${gaps:-none}"
jq -r '.checks[] | "  \(.verdict)\t\(.name)\t\(.issue // "")"' "$OUT/drill-result.json"
jq -r '"RTO \(.timings.rtoSeconds) s, RPO \(.timings.rpoSeconds) s (recorded, no targets)"' "$OUT/drill-result.json"
verdict=$(jq -r '.verdict' "$OUT/drill-result.json")
echo "verdict: $verdict"
if [[ $verdict != pass ]] || (( unexpected > 0 || ran == 0 )); then exit 1; fi
status=0
