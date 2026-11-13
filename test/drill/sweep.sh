#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The drill's teardown (ADR 0109): deletes what one drill run made, by the label
# koinon.dev/drill-run=<id>, and refuses anything without it.
#
#   KUBECONFIG=<file> test/drill/sweep.sh <run-id> [--context <ctx>]
#   KUBECONFIG=<file> test/drill/sweep.sh --list <run-id>      what it would delete, nothing deleted
#
# --list shows every labelled namespace with its phase, a Terminating one too (it is still there and
# may hold a finalizer). The sweep then waits until the labelled and workspace namespaces are gone, up
# to SWEEP_TIMEOUT seconds (default 300, polled every SWEEP_POLL seconds, default 5), and exits 1
# when some are left.
#
# It deletes, in this order: Helm releases in the labelled namespaces, labelled cluster-scoped objects
# (the chart's ClusterRoles and admission policies), labelled Secrets in any namespace (the install-mark
# Secrets survive `helm uninstall`), then the labelled namespaces and the
# OpenShell workspace namespaces of the run's two gateway IDs. A namespace that does not start with
# koinon-drill- is refused even when labelled, and an object without the label is never selected. The
# suite's DeferCleanup removes what it made as it goes; this script is the backstop (run.sh calls it
# in a trap, CI in an always() step).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

list_only=0
if [[ ${1:-} == --list ]]; then list_only=1; shift; fi
run=${1:-}
shift || true
ctx=""
if [[ ${1:-} == --context ]]; then ctx=${2:?--context needs a value}; fi
[[ $run =~ ^[a-z0-9]{3,12}$ ]] || { echo "usage: sweep.sh [--list] <run-id> [--context <ctx>]  (run ID: 3 to 12 lowercase letters and digits)" >&2; exit 2; }
[[ -n ${KUBECONFIG:-} && $KUBECONFIG != /dev/null ]] || { echo "sweep.sh needs an explicit KUBECONFIG" >&2; exit 2; }

label="koinon.dev/drill-run=$run"
k=(kubectl)
h=(helm)
if [[ -n $ctx ]]; then k+=(--context "$ctx"); h+=(--kube-context "$ctx"); fi

# ns_phases SELECTOR: "<name> <phase>" per line, every namespace the selector matches (Terminating too).
ns_phases() { "${k[@]}" get namespace -l "$1" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.phase}{"\n"}{end}'; }
ns_names() { awk 'NF { print $1 }'; }

ns_lines=$(ns_phases "$label")
namespaces=$(ns_names <<<"$ns_lines")
for ns in $namespaces; do
  case $ns in
    koinon-drill-*) ;;
    *) echo "REFUSED: namespace $ns carries $label but does not start with koinon-drill-" >&2; exit 1 ;;
  esac
done

# The OpenShell workspace namespaces the two gateways of this run manage (openshell-<gatewayId>-<workspace>).
# The gateway creates them, so they carry no run label; they are found by the gateway-id label of the run's
# own IDs and only when the name has the matching prefix.
os_namespaces=""
os_lines=""
for side in src dst; do
  gid="drill-$side-$run"
  gid_lines=$(ns_phases "openshell.ai/gateway-id=$gid")
  for ns in $(ns_names <<<"$gid_lines"); do
    case $ns in
      "openshell-$gid-"*) os_namespaces+="$ns "; os_lines+="$(grep -E "^$ns " <<<"$gid_lines")"$'\n' ;;
      *) echo "REFUSED: namespace $ns carries gateway-id $gid but is not named openshell-$gid-*" >&2; exit 1 ;;
    esac
  done
done

if (( list_only )); then
  echo "namespaces:"
  if [[ -n $namespaces ]]; then printf '  %s\n' "${ns_lines//$'\n'/$'\n  '}"; else echo "  none"; fi
  echo "openshell workspace namespaces:"
  if [[ -n $os_namespaces ]]; then sed '/^$/d; s/^/  /' <<<"$os_lines"; else echo "  none"; fi
  "${k[@]}" get clusterrole,clusterrolebinding,validatingadmissionpolicy,validatingadmissionpolicybinding -l "$label" -o name || true
  "${k[@]}" get secret -A -l "$label" -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name --no-headers || true
  exit 0
fi

for ns in $namespaces; do
  for rel in $("${h[@]}" list -n "$ns" -q 2>/dev/null || true); do
    echo "helm uninstall $rel -n $ns"
    "${h[@]}" uninstall "$rel" -n "$ns" --wait=false || true
  done
done
"${k[@]}" delete clusterrolebinding,clusterrole,validatingadmissionpolicybinding,validatingadmissionpolicy -l "$label" --ignore-not-found --wait=false
# Secrets with the label, wherever they are; the namespaces below take the rest.
"${k[@]}" delete secret -A -l "$label" --ignore-not-found --wait=false
if [[ -n $os_namespaces ]]; then
  # shellcheck disable=SC2086 # the names are one word each
  "${k[@]}" delete namespace $os_namespaces --ignore-not-found --wait=false
fi
if [[ -n $namespaces ]]; then
  # shellcheck disable=SC2086 # the names are one word each
  "${k[@]}" delete namespace $namespaces --ignore-not-found --wait=false
fi
# Wait for the namespaces to go: a Terminating namespace is still there until its finalizers finish.
timeout=${SWEEP_TIMEOUT:-300}
poll=${SWEEP_POLL:-5}
[[ $timeout =~ ^[0-9]+$ && $poll =~ ^[0-9]+$ ]] || { echo "SWEEP_TIMEOUT and SWEEP_POLL are whole seconds" >&2; exit 2; }
deadline=$((SECONDS + timeout))
left=""
poll_err=""
# One poll of the label selector and of both OpenShell workspace selectors. A failed kubectl call (an
# API blip) counts as "not gone yet": its stderr is kept for the timeout report and the wait goes on.
# Returns 1 when any call failed (left then holds what the calls that worked saw, plus a marker).
poll_left() {
  local out err_file failed=0 side
  err_file=$(mktemp "${TMPDIR:-/tmp}/sweep-poll.XXXXXX")
  left=""
  poll_err=""
  if out=$(ns_phases "$label" 2>"$err_file"); then left=$out; else failed=1; poll_err+=$(<"$err_file"); fi
  for side in src dst; do
    if out=$(ns_phases "openshell.ai/gateway-id=drill-$side-$run" 2>"$err_file"); then
      left+=$'\n'$(grep -E "^openshell-drill-$side-$run-" <<<"$out" || true)
    else
      failed=1; poll_err+=$(<"$err_file")
    fi
  done
  rm -f "$err_file"
  left=$(sed '/^$/d' <<<"$left")
  return "$failed"
}
while :; do
  poll_ok=1
  poll_left || poll_ok=0
  # Only a clean poll that finds nothing ends the wait.
  [[ $poll_ok == 1 && -z $left ]] && break
  if (( SECONDS >= deadline )); then
    echo "sweep $run: namespace(s) still present after ${timeout}s:" >&2
    if [[ -n $left ]]; then printf '  %s\n' "${left//$'\n'/$'\n  '}" >&2; else echo "  (none seen; the last poll failed)" >&2; fi
    [[ -z $poll_err ]] || printf 'last poll error: %s\n' "$poll_err" >&2
    exit 1
  fi
  sleep "$poll"
done
echo "sweep $run: no labelled namespaces left"
