#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The escape suite's OpenShell cases (ADR 0025) against the OpenShell gateway that
# `make dev-kind-up` installs on kind-koinon-dev (or DEV_KIND_CLUSTER). No registry, no home lab:
# the probe image is built from hack/spike/probe and loaded into the node; the gateway is reached
# through a port-forward with the client certificate it generated.
#
# kind runs sandboxes under runc, so the Kata guest case skips (it needs KOINON_ESCAPE_HOST_KERNEL
# and a Kata node, the maintainer's release gate). Cases that need other fixtures skip with their
# reason; the rest run. Every kubectl call names the kind context and nothing else is touched.
set -euo pipefail

CLUSTER=${CLUSTER:-${DEV_KIND_CLUSTER:-koinon-dev}}
CTX="kind-${CLUSTER}"
PORT=${KOINON_ESCAPE_KIND_PORT:-18080}
IMAGE=${KOINON_OPENSHELL_IMAGE:-koinon/spike-probe:1}
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"

kind get clusters 2>/dev/null | grep -qx "$CLUSTER" || { echo "no kind cluster $CLUSTER: run 'make dev-kind-up' first" >&2; exit 1; }
K=(kubectl --context "$CTX")
"${K[@]}" -n openshell get secret openshell-client-tls >/dev/null ||
  { echo "the OpenShell gateway is not installed in $CTX (make dev-kind-up installs it)" >&2; exit 1; }

tmp=$(mktemp -d)
pf=
trap '[[ -z $pf ]] || { kill "$pf" 2>/dev/null; wait "$pf" 2>/dev/null; } || true; rm -rf "$tmp"' EXIT

echo "== probe image $IMAGE (hack/spike/probe)"
docker build -q -t "$IMAGE" "$root/hack/spike/probe" >/dev/null
kind load docker-image --name "$CLUSTER" "$IMAGE"

echo "== client certificate"
for k in tls.crt tls.key ca.crt; do
  "${K[@]}" -n openshell get secret openshell-client-tls -o json |
    jq -r --arg k "$k" '.data[$k]' | base64 -d >"$tmp/$k"
done

echo "== port-forward svc/openshell to 127.0.0.1:$PORT"
"${K[@]}" -n openshell port-forward svc/openshell "$PORT:8080" >"$tmp/pf.log" 2>&1 &
pf=$!
for _ in $(seq 1 30); do
  (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null && break
  kill -0 "$pf" 2>/dev/null || { cat "$tmp/pf.log" >&2; exit 1; }
  sleep 1
done

echo "== escape suite, OpenShell cases"
cd "$root"
KOINON_OPENSHELL_ENDPOINT="127.0.0.1:$PORT" \
KOINON_OPENSHELL_MTLS_DIR="$tmp" \
KOINON_OPENSHELL_IMAGE="$IMAGE" \
  go test -count=1 -v -timeout 20m ./test/escape/ -ginkgo.label-filter=openshell
