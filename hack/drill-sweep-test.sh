#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of test/drill/sweep.sh with a fake kubectl and helm: a Terminating namespace is listed
# with its phase, and the sweep waits for it and fails after SWEEP_TIMEOUT when it stays.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

script=$(cd "$(dirname "$0")/.." && pwd)/test/drill/sweep.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/drill-sweep-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

mkdir "$tmp/bin"
cat >"$tmp/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
# fake kubectl: namespaces live in $FAKE_DIR/ns-run ("<name> <phase>" per line); the OpenShell label
# selector finds nothing. `delete namespace` empties the file unless FAKE_STUCK=1 (a finalizer holds it,
# the phase becomes Terminating); FAKE_GONE_AFTER=N makes the namespace go after N more `get` calls.
echo "kubectl $*" >>"$FAKE_DIR/calls"
args=" $* "
case $args in
*" get namespace "*)
	# FAKE_FAIL_POLLS=N: the first N `get namespace` calls after a delete matching FAKE_FAIL_ON fail (API blip).
	if [[ -f $FAKE_DIR/deleted && -n ${FAKE_FAIL_POLLS:-} && $args == *"${FAKE_FAIL_ON:-namespace}"* ]]; then
		done=$(cat "$FAKE_DIR/failed" 2>/dev/null || echo 0)
		if ((done < FAKE_FAIL_POLLS)); then echo $((done + 1)) >"$FAKE_DIR/failed"; echo "fake: connection refused" >&2; exit 1; fi
	fi
	case $args in *" -l koinon.dev/drill-run="*) ;; *) exit 0 ;; esac
	if [[ -f $FAKE_DIR/gone-after ]]; then
		n=$(<"$FAKE_DIR/gone-after")
		if ((n <= 0)); then : >"$FAKE_DIR/ns-run"; else echo $((n - 1)) >"$FAKE_DIR/gone-after"; fi
	fi
	cat "$FAKE_DIR/ns-run" ;;
*" delete namespace "*)
	: >"$FAKE_DIR/deleted"
	if [[ ${FAKE_STUCK:-0} == 1 ]]; then sed -i.bak 's/ Active$/ Terminating/' "$FAKE_DIR/ns-run"
	elif [[ -n ${FAKE_GONE_AFTER:-} ]]; then echo "$FAKE_GONE_AFTER" >"$FAKE_DIR/gone-after"; sed -i.bak 's/ Active$/ Terminating/' "$FAKE_DIR/ns-run"
	else : >"$FAKE_DIR/ns-run"; fi ;;
esac
exit 0
FAKE
printf '#!/usr/bin/env bash\nexit 0\n' >"$tmp/bin/helm"
chmod +x "$tmp/bin/kubectl" "$tmp/bin/helm"
export PATH="$tmp/bin:$PATH" FAKE_DIR="$tmp" KUBECONFIG="$tmp/kubeconfig" SWEEP_POLL=1
: >"$KUBECONFIG"

fails=0
check() { # NAME CONDITION-EXIT
	if (($2 == 0)); then echo "ok: $1"; else echo "FAIL: $1"; fails=1; fi
}
reset() { printf '%s\n' "$@" >"$tmp/ns-run"; rm -f "$tmp/gone-after" "$tmp/calls" "$tmp/deleted" "$tmp/failed"; unset FAKE_STUCK FAKE_GONE_AFTER FAKE_FAIL_POLLS FAKE_FAIL_ON; }

# --list shows a Terminating namespace with its phase, and deletes nothing.
reset "koinon-drill-abc1 Active" "koinon-drill-abc1-b Terminating"
out=$("$script" --list abc1 2>&1)
grep -q 'koinon-drill-abc1-b Terminating' <<<"$out"; check "--list shows the Terminating namespace with its phase" $?
grep -q 'koinon-drill-abc1 Active' <<<"$out"; check "--list shows the Active namespace with its phase" $?
if grep -q delete "$tmp/calls"; then check "--list deletes nothing" 1; else check "--list deletes nothing" 0; fi

# --list with nothing prints none.
reset
out=$("$script" --list abc1 2>&1)
grep -q 'none' <<<"$out"; check "--list prints none when nothing is labelled" $?

# The sweep succeeds once the namespace is gone.
reset "koinon-drill-abc1 Active"
SWEEP_TIMEOUT=10 "$script" abc1 >"$tmp/out" 2>&1; check "sweep succeeds when the namespaces go" $?

# A namespace that stays Terminating makes the sweep wait, then exit 1 naming it.
reset "koinon-drill-abc1 Active"
rc=0; FAKE_STUCK=1 SWEEP_TIMEOUT=2 "$script" abc1 >"$tmp/out" 2>&1 || rc=$?
((rc == 1)); check "sweep exits 1 when a namespace stays Terminating" $?
grep -q 'koinon-drill-abc1 Terminating' "$tmp/out"; check "the failure names the stuck namespace and phase" $?

# A namespace that goes after a few polls is waited for.
reset "koinon-drill-abc1 Active"
FAKE_GONE_AFTER=2 SWEEP_TIMEOUT=20 "$script" abc1 >"$tmp/out" 2>&1; check "sweep waits for a Terminating namespace that goes" $?
# One discovery poll plus at least two wait polls of the label selector (the OpenShell lookups do not count).
(($(grep -c 'get namespace -l koinon.dev/drill-run=' "$tmp/calls") > 2)); check "the sweep polled the label more than once" $?

# One failed poll is "not gone yet": the sweep goes on and exits 0 once a later poll shows nothing.
for on in koinon.dev/drill-run= openshell.ai/gateway-id=; do
	reset "koinon-drill-abc1 Active"
	rc=0; FAKE_FAIL_POLLS=1 FAKE_FAIL_ON=$on SWEEP_TIMEOUT=20 "$script" abc1 >"$tmp/out" 2>&1 || rc=$?
	((rc == 0)); check "sweep survives one failed poll ($on)" $?
	[[ $(cat "$tmp/failed") == 1 ]]; check "the poll really failed once ($on)" $?
done

# Polls that keep failing end at the timeout with exit 1 and the error, not a silent abort.
reset "koinon-drill-abc1 Active"
rc=0; FAKE_FAIL_POLLS=100 SWEEP_TIMEOUT=2 "$script" abc1 >"$tmp/out" 2>&1 || rc=$?
((rc == 1)); check "sweep exits 1 at the timeout when every poll fails" $?
grep -q 'connection refused' "$tmp/out"; check "the timeout report carries the poll error" $?

((fails == 0)) || exit 1
echo "all drill-sweep tests passed"
