#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of test/drill/run.sh (#1016) with a fake kubectl, helm, kind and go: no cluster, no image, no
# suite. The fake kubectl keeps one set of namespaces per kube context and acts on the kubeconfig's current
# context when a call has no --context, which is the bug being tested for. The context itself is read by the
# real loader (`go run ./test/drill/topctx`), so the test covers the Go side too.

# shellcheck disable=SC2319 # check reads the status of the test just before it, on purpose

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/drill-run-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

mkdir "$tmp/bin"
cat >"$tmp/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
# fake kubectl: $FAKE_DIR/<context>/ns holds the namespaces of that context, one name per line.
ctx=""
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
	[[ ${args[i]} == --context ]] && ctx=${args[i + 1]}
done
if [[ -z $ctx ]]; then ctx=$(sed -n 's/^current-context: //p' "$KUBECONFIG"); echo "NOCONTEXT kubectl $*" >>"$FAKE_DIR/calls"; fi
echo "ctx=$ctx kubectl $*" >>"$FAKE_DIR/calls"
mkdir -p "$FAKE_DIR/$ctx"
ns=$FAKE_DIR/$ctx/ns
touch "$ns"
all=" $* "
case $all in
*" get namespace -l koinon.dev/drill-run="*)
	case $all in
	*.status.phase*) sed 's/$/ Active/' "$ns" ;;
	*" -o name "*) sed 's#^#namespace/#' "$ns" ;;
	*) cat "$ns" ;;
	esac ;;
*" get pods "*) echo "pod-of-this-run" ;;
*" get events "*) echo "event-of-this-run" ;;
*" delete namespace "*) [[ ${FAKE_STUCK:-0} == 1 ]] || : >"$ns" ;;
esac
exit 0
FAKE
cat >"$tmp/bin/helm" <<'FAKE'
#!/usr/bin/env bash
echo "helm $*" >>"$FAKE_DIR/calls"
[[ " $* " == *" list "* ]] && echo rel1
exit 0
FAKE
# fake kind: only `get kubeconfig` and `delete cluster` are used (the kind path with SKIP_UP and SKIP_BUILD).
cat >"$tmp/bin/kind" <<'FAKE'
#!/usr/bin/env bash
echo "kind $*" >>"$FAKE_DIR/kindcalls"
[[ $1 == get ]] && cat "$FAKE_DIR/kubeconfig"
exit 0
FAKE
# fake go: `go test -c -o X dir` writes X, a stand-in for the drill suite that leaves a result and a report.
cat >"$tmp/bin/go" <<'FAKE'
#!/usr/bin/env bash
[[ $1 == run ]] && exec "$REAL_GO" "$@"
if [[ ${FAKE_GO_FAILS:-0} == 1 ]]; then echo "fake: build failed" >&2; exit 1; fi
out=""
for ((i = 1; i <= $#; i++)); do [[ ${!i} == -o ]] && { j=$((i + 1)); out=${!j}; }; done
cat >"$out" <<'SUITE'
#!/usr/bin/env bash
echo '[{"SpecReports":[{"LeafNodeType":"It","State":"passed"}]}]' >"$KOINON_DRILL_OUT/report.json"
cp "$FAKE_RESULT" "$KOINON_DRILL_OUT/drill-result.json"
SUITE
chmod +x "$out"
FAKE
chmod +x "$tmp/bin/kubectl" "$tmp/bin/helm" "$tmp/bin/go" "$tmp/bin/kind"
REAL_GO=$(command -v go) || { echo "go is needed" >&2; exit 2; }
export REAL_GO
export PATH="$tmp/bin:$PATH" FAKE_DIR="$tmp" SWEEP_POLL=1 SWEEP_TIMEOUT=3 TMPDIR="$tmp"

# A kubeconfig with two contexts; the current one is NOT the topology's.
export KUBECONFIG="$tmp/kubeconfig"
cat >"$KUBECONFIG" <<'KC'
apiVersion: v1
kind: Config
current-context: other-ctx
contexts:
- name: other-ctx
- name: drill-ctx
KC
# The example cluster topology with its context replaced (quoted, with a comment: the loader must cope).
sed 's/^context: .*/context: "drill-ctx"   # the topology'"'"'s context/' "$root/test/drill/topology/cluster.example.yaml" >"$tmp/topology.yaml"
jq -n '{runId: "abc123", verdict: "pass", timings: {rtoSeconds: 1, rpoSeconds: 1}, knownGaps: [], checks: [{name: "a", verdict: "pass"}]}' >"$tmp/result-green.json"

fails=0
check() { # NAME CONDITION-EXIT
	if (($2 == 0)); then echo "ok: $1"; else echo "FAIL: $1"; fails=1; [[ -z ${DRILL_TEST_DEBUG:-} ]] || tail -15 "$tmp/run.log"; fi
}
reset() {
	rm -rf "$tmp/drill-ctx" "$tmp/other-ctx" "$tmp/kind-other" "$tmp/calls" "$tmp/kindcalls" "$tmp/out"
	mkdir -p "$tmp/drill-ctx" "$tmp/other-ctx" "$tmp/kind-other"
	echo koinon-drill-abc123 >"$tmp/drill-ctx/ns"
	echo koinon-drill-abc123 >"$tmp/other-ctx/ns"
	echo koinon-drill-abc123 >"$tmp/kind-other/ns"
	touch "$tmp/calls"
	unset FAKE_STUCK FAKE_GO_FAILS
	export FAKE_RESULT=$tmp/result-green.json
}
run() { # [ENV=VAL...]: run.sh against the fake cluster topology (an external cluster)
	env DRILL_TOPOLOGY="$tmp/topology.yaml" DRILL_RUN=abc123 OUT="$tmp/out" "$@" bash "$root/test/drill/run.sh" >"$tmp/run.log" 2>&1
}
run_kind() { # [ENV=VAL...]: run.sh on kind (it copies the kind topology and makes no cluster here)
	env -u DRILL_TOPOLOGY DRILL_RUN=abc123 OUT="$tmp/out" SKIP_UP=1 SKIP_BUILD=1 KIND_REGISTRY=localhost:5001 "$@" bash "$root/test/drill/run.sh" >"$tmp/run.log" 2>&1
}
all_pinned() { # CONTEXT: every kubectl call has --context CONTEXT, every helm call --kube-context CONTEXT
	! grep -q NOCONTEXT "$tmp/calls" &&
		! grep '^ctx=' "$tmp/calls" | grep -v "^ctx=$1 " | grep -q . &&
		! grep '^helm' "$tmp/calls" | grep -qv -- "^helm --kube-context $1 " &&
		! grep '^ctx=' "$tmp/calls" | grep -qv -- "kubectl .*--context $1 "
}

# The exit-trap sweep acts on the topology's context, not the kubeconfig's current one.
reset
FAKE_GO_FAILS=1 run; rc=$?
((rc == 1)); check "a failed run exits 1" $?
[[ ! -s $tmp/drill-ctx/ns ]]; check "the trap sweep deleted the run's namespace on the topology's context" $?
[[ -s $tmp/other-ctx/ns ]]; check "...and left the current context's namespace alone" $?
all_pinned drill-ctx; check "every kubectl call has --context and every helm call --kube-context (drill-ctx)" $?
grep -q '^helm --kube-context drill-ctx uninstall rel1' "$tmp/calls"; check "...helm uninstall among them" $?
grep -q 'get pods -A' "$tmp/calls"; check "the diagnostics ran (and carry the context too)" $?

# A green run exits 0 and acts on the topology's context only.
reset
run; rc=$?
((rc == 0)); check "a green run exits 0" $?
all_pinned drill-ctx; check "...every call pinned to the topology's context" $?

# A sweep that cannot finish (a namespace stays): one line, non-zero exit, drill-result.json untouched.
reset
sum=$(cp "$tmp/result-green.json" "$tmp/expect.json" && shasum "$tmp/expect.json" | cut -d' ' -f1)
FAKE_STUCK=1 run; rc=$?
((rc == 1)); check "a failed sweep on an external cluster exits 1" $?
(($(grep -c '^FAIL: the sweep of run abc123 on context drill-ctx' "$tmp/run.log") == 1)); check "...with one clear line" $?
[[ $(shasum "$tmp/out/drill-result.json" | cut -d' ' -f1) == "$sum" ]]; check "...and drill-result.json is not changed" $?

# KEEP=1: nothing deleted.
reset
run KEEP=1; rc=$?
((rc == 0)); check "KEEP=1 run exits 0" $?
[[ -s $tmp/drill-ctx/ns ]]; check "KEEP=1 leaves the namespaces" $?

# A topology that does not load (no context) is refused before anything runs.
reset
grep -v '^context:' "$root/test/drill/topology/cluster.example.yaml" >"$tmp/nocontext.yaml"
DRILL_TOPOLOGY="$tmp/nocontext.yaml" DRILL_RUN=abc123 OUT="$tmp/out2" bash "$root/test/drill/run.sh" >"$tmp/run.log" 2>&1; rc=$?
((rc == 2)); check "a topology with no context is refused (exit 2)" $?
[[ ! -s $tmp/calls ]]; check "...and touches no cluster" $?

# kind: the copied topology's context follows CLUSTER, and the sweep uses it.
reset
cat >"$tmp/kubeconfig-kind" <<'KC'
apiVersion: v1
kind: Config
current-context: other-ctx
KC
cp "$tmp/kubeconfig-kind" "$tmp/kubeconfig"
FAKE_GO_FAILS=1 run_kind CLUSTER=other; rc=$?
grep -q '^context: kind-other$' "$tmp/out/topology.yaml"; check "kind: CLUSTER=other gives the copied topology 'context: kind-other'" $?
all_pinned kind-other; check "...and every call is pinned to kind-other" $?
[[ ! -s $tmp/kind-other/ns && -s $tmp/other-ctx/ns ]]; check "...the sweep acted on kind-other only" $?
# On kind a failed sweep stays outside the red signal: the cluster is deleted anyway.
reset
cp "$tmp/kubeconfig-kind" "$tmp/kubeconfig"
FAKE_STUCK=1 run_kind CLUSTER=other; rc=$?
((rc == 0)); check "kind: a failed sweep does not turn a green run red" $?
grep -q '^FAIL: the sweep of run abc123 on context kind-other' "$tmp/run.log"; check "...but says so in one line" $?
grep -q 'delete cluster --name other' "$tmp/kindcalls"; check "...and the kind cluster is still deleted" $?

((fails == 0)) || exit 1
echo "all drill-run tests passed"
