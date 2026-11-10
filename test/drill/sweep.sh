#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The drill's teardown (ADR 0109): deletes what one drill run made, by the label
# koinon.dev/drill-run=<id>, and refuses anything without it.
#
#   KUBECONFIG=<file> test/drill/sweep.sh <run-id> [--context <ctx>]
#   KUBECONFIG=<file> test/drill/sweep.sh --list <run-id>      what it would delete, nothing deleted
#
# It deletes, in this order: Helm releases in the labelled namespaces, labelled cluster-scoped objects
# (the chart's ClusterRoles and admission policies), labelled Secrets in any namespace (the install-mark
# Secrets survive `helm uninstall`), then the labelled namespaces. A namespace that does not start with
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

namespaces=$("${k[@]}" get namespace -l "$label" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
for ns in $namespaces; do
  case $ns in
    koinon-drill-*) ;;
    *) echo "REFUSED: namespace $ns carries $label but does not start with koinon-drill-" >&2; exit 1 ;;
  esac
done

if (( list_only )); then
  echo "namespaces: ${namespaces:-none}"
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
if [[ -n $namespaces ]]; then
  # shellcheck disable=SC2086 # the names are one word each
  "${k[@]}" delete namespace $namespaces --ignore-not-found --wait=false
fi
left=$("${k[@]}" get namespace -l "$label" -o name 2>/dev/null | wc -l | tr -d ' ')
echo "sweep $run: $left labelled namespace(s) still terminating"
