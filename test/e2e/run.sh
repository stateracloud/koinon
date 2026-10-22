#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# E1 quickstart e2e on kind (plan item E1, ADR 0049). From nothing to an approved pull request:
#
#   1. prerequisites (not on the quickstart clock): kind/up.sh;
#   2. images: Koinon, the fake model server and the fake GitHub with ko, the OpenCode flavor
#      with buildx, all loaded into the kind node; the fakes deployed in koinon-e2e-fakes;
#   3. `helm install` of charts/koinon (runc, CNPG, the fakes as model and GitHub);
#   4. the Go driver in test/e2e drives the quickstart steps through the API and prints the wall
#      clock of each step and the total from `helm install`.
#
# Only the kube context kind-$CLUSTER is used; every kubectl/helm call names it. The cluster is
# deleted at the end unless KEEP=1.
#
#   CLUSTER=koinon-e1  KEEP=0  SKIP_UP=0  SKIP_BUILD=0  OUT=<dir for logs and timings>
set -euo pipefail

CLUSTER=${CLUSTER:-koinon-e1}
CTX="kind-${CLUSTER}"
NS=koinon
FAKES=koinon-e2e-fakes
KEEP=${KEEP:-0}
SKIP_UP=${SKIP_UP:-0}
SKIP_BUILD=${SKIP_BUILD:-0}
API_PORT=${API_PORT:-18080}
GH_PORT=${GH_PORT:-18090}

root="$(cd "$(dirname "$0")/../.." && pwd)"
here="$root/test/e2e"
OUT=${OUT:-$(mktemp -d)}
mkdir -p "$OUT"
K=(kubectl --context "$CTX")
H=(helm --kube-context "$CTX")
cd "$root"

pids=()
status=1
summary=()
step() { echo "== $(date -u +%H:%M:%SZ) $*"; ts=$(date +%s); }
done_step() {
  local d=$(( $(date +%s) - ts ))
  echo "-- $1: ${d}s"
  summary+=("| $1 | ${d} s |")
}

diagnostics() {
  local d="$OUT/diagnostics"
  mkdir -p "$d"
  "${K[@]}" get pods -A -o wide >"$d/pods.txt" 2>&1 || true
  "${K[@]}" get events -A --sort-by=.lastTimestamp >"$d/events.txt" 2>&1 || true
  for c in api reconciler agentd gateway; do
    "${K[@]}" -n "$NS" logs "deploy/koinon-$c" --all-containers --tail=-1 >"$d/koinon-$c.log" 2>&1 || true
  done
  for c in fakemodel fakegithub; do
    "${K[@]}" -n "$FAKES" logs "deploy/$c" --tail=-1 >"$d/$c.log" 2>&1 || true
  done
  "${K[@]}" -n openshell logs statefulset/openshell --tail=-1 >"$d/openshell.log" 2>&1 || true
  echo "diagnostics in $d"
}

cleanup() {
  for p in ${pids[@]+"${pids[@]}"}; do kill "$p" 2>/dev/null || true; done
  if [[ $status -ne 0 ]]; then diagnostics; fi
  if [[ "$KEEP" != 1 ]]; then "$here/kind/down.sh" || true; fi
  echo "logs and timings in $OUT"
  exit "$status"
}
trap cleanup EXIT

t_all=$(date +%s)

if [[ "$SKIP_UP" != 1 ]]; then
  step "prerequisites (kind/up.sh)"
  CLUSTER="$CLUSTER" "$here/kind/up.sh" 2>&1 | tee "$OUT/up.log"
  done_step "Prerequisites: kind, kube-network-policies, CNPG, Agent Sandbox, runc, OpenShell"
fi

if [[ "$SKIP_BUILD" != 1 ]]; then
  arch=$(go env GOARCH)
  step "Koinon images (ko, linux/$arch) into kind"
  KO_DOCKER_REPO=kind.local KIND_CLUSTER_NAME="$CLUSTER" "${KO:-ko}" build --base-import-paths --tags e2e \
    --platform "linux/$arch" ./cmd/koinon-api ./cmd/koinon-reconciler ./cmd/koinon-agentd \
    ./cmd/koinon-fakemodel ./cmd/koinon-fakegithub
  done_step "Images: ko build and load (5 binaries)"

  step "OpenCode flavor (buildx) into kind"
  docker buildx build --load --build-context common=flavors/common \
    --tag kind.local/flavor-opencode:e2e flavors/opencode
  kind load docker-image --name "$CLUSTER" kind.local/flavor-opencode:e2e
  done_step "Image: OpenCode flavor buildx and kind load"
fi

step "fakes ($FAKES) and the OpenShell client certificate"
"${K[@]}" apply -f "$here/kind/fakes.yaml"
"${K[@]}" -n "$FAKES" rollout restart deploy/fakemodel deploy/fakegithub >/dev/null
"${K[@]}" -n "$FAKES" rollout status deploy/fakemodel deploy/fakegithub --timeout=120s
"${K[@]}" create namespace "$NS" --dry-run=client -o yaml | "${K[@]}" apply -f -
# The OpenShell chart generates the client bundle in its own namespace; Koinon mounts a copy.
"${K[@]}" -n openshell get secret openshell-client-tls -o json \
  | jq --arg ns "$NS" '{apiVersion, kind, type, data, metadata: {name: .metadata.name, namespace: $ns}}' \
  | "${K[@]}" apply -f -
done_step "Fakes deployed, OpenShell client certificate copied"

# The quickstart clock starts here (ADR 0049).
extra=()
if grep -q '^preflight:' charts/koinon/values.yaml; then
  # Plan item H2. kind has no Kata, so the guest seccomp probe does not apply on runc.
  extra+=(--set preflight.enabled=true --set preflight.seccompProbe.enabled=false)
fi
step "helm install koinon"
T0=$(date +%s)
"${H[@]}" install koinon charts/koinon --namespace "$NS" -f "$here/kind/koinon-values.yaml" \
  ${extra[@]+"${extra[@]}"} --wait --timeout 10m
done_step "helm install --wait"

"${K[@]}" -n "$NS" port-forward svc/koinon-api "$API_PORT:8080" >"$OUT/pf-api.log" 2>&1 &
pids+=($!)
"${K[@]}" -n "$FAKES" port-forward svc/fakegithub "$GH_PORT:8080" >"$OUT/pf-fakegithub.log" 2>&1 &
pids+=($!)
for port in "$API_PORT" "$GH_PORT"; do
  for _ in $(seq 60); do nc -z 127.0.0.1 "$port" 2>/dev/null && break; sleep 0.5; done
  nc -z 127.0.0.1 "$port" || { echo "port-forward to $port did not come up" >&2; exit 1; }
done

step "quickstart driver (go test ./test/e2e)"
password=$("${K[@]}" -n "$NS" get secret koinon-bootstrap-admin -o jsonpath='{.data.password}' | base64 -d)
KOINON_E2E_API_URL="http://127.0.0.1:$API_PORT" \
KOINON_E2E_FAKEGITHUB_URL="http://127.0.0.1:$GH_PORT" \
KOINON_E2E_PASSWORD="$password" \
KOINON_E2E_T0="$T0" \
KOINON_E2E_REPORT="$OUT/quickstart-timings.md" \
  go test -count=1 -timeout 30m ./test/e2e/ -ginkgo.v 2>&1 | tee "$OUT/driver.log"
done_step "Quickstart driver"

status=0
echo
echo "| Phase | Wall clock |"
echo "| --- | --- |"
printf '%s\n' "${summary[@]}"
echo "| **Whole script** | **$(( $(date +%s) - t_all )) s** |"
echo
echo "Quickstart steps (from helm install):"
cat "$OUT/quickstart-timings.md"
