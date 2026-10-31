#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The escape suite's OpenShell cases (ADR 0025) against the OpenShell gateway that
# `make dev-kind-up` installs on kind-koinon-dev (or DEV_KIND_CLUSTER). No registry, no home lab:
# the probe image is built from hack/spike/probe (or, when KOINON_OPENSHELL_IMAGE names an image that
# pulls, e.g. the published ghcr.io/stateracloud/koinon-spike-probe by digest, pulled instead: no
# nvcr.io or apt at run time, #357) and loaded into the node; the gateway is reached through a
# port-forward with the client certificate it generated.
#
# The gateway-route case needs a stand-in for agentgateway on port 4000 (ADR 0080, #270): unless
# KOINON_ESCAPE_GATEWAY_HOST already names one, or KOINON_ESCAPE_STANDIN=0, it is built from
# cmd/koinon-fakemodel with ko and run in namespace koinon-fixtures, as the RT1 fixtures do.
#
# The Gitea read-grant specs need a TLS server behind the host their grant names, because the proxy
# dials an allowed host before it judges the request (#371). Unless KOINON_ESCAPE_GITEA_URL is set,
# or KOINON_ESCAPE_STANDIN=0, a stand-in (koinon-fakemodel -standin over TLS, in koinon-fixtures)
# gets a certificate from a throwaway CA, and the probe image the sandboxes run is rebuilt with that
# CA in its trust store so the proxy's upstream handshake verifies. Nothing leaves the cluster.
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

# Ambient settings must not point the suite's cluster-side cases at another cluster.
unset KOINON_ESCAPE_NAMESPACE KOINON_ESCAPE_KUBECONFIG
export KOINON_ESCAPE_KUBE_CONTEXT="$CTX"

kind get clusters 2>/dev/null | grep -qx "$CLUSTER" || { echo "no kind cluster $CLUSTER: run 'make dev-kind-up' first" >&2; exit 1; }
K=(kubectl --context "$CTX")
"${K[@]}" -n openshell get secret openshell-client-tls >/dev/null ||
  { echo "the OpenShell gateway is not installed in $CTX (make dev-kind-up installs it)" >&2; exit 1; }

tmp=$(mktemp -d)
pf=
trap '[[ -z $pf ]] || { kill "$pf" 2>/dev/null; wait "$pf" 2>/dev/null; } || true; rm -rf "$tmp"' EXIT

if [[ -n ${KOINON_OPENSHELL_IMAGE:-} ]] && docker pull -q "$IMAGE" >"$tmp/pull.log" 2>&1; then
  # kind loads by name: a digest-only reference gets a local tag the sandboxes then use.
  if [[ $IMAGE == *@* ]]; then
    docker tag "$IMAGE" koinon/spike-probe:pulled
    IMAGE=koinon/spike-probe:pulled
  fi
  echo "== probe image $IMAGE (pulled $KOINON_OPENSHELL_IMAGE)"
else
  if [[ -n ${KOINON_OPENSHELL_IMAGE:-} ]]; then
    echo "== could not pull $KOINON_OPENSHELL_IMAGE ($(tail -1 "$tmp/pull.log")); building it instead"
    [[ $IMAGE != *@* ]] || IMAGE=koinon/spike-probe:1
  fi
  echo "== probe image $IMAGE (built from hack/spike/probe)"
  docker build -q -t "$IMAGE" "$root/hack/spike/probe" >/dev/null
fi
kind load docker-image --name "$CLUSTER" "$IMAGE"

need_gateway=0 need_gitea=0
[[ -z ${KOINON_ESCAPE_GATEWAY_HOST:-} && ${KOINON_ESCAPE_STANDIN:-1} != 0 ]] && need_gateway=1
[[ -z ${KOINON_ESCAPE_GITEA_URL:-} && ${KOINON_ESCAPE_STANDIN:-1} != 0 ]] && need_gitea=1

if (( need_gateway || need_gitea )); then
  echo "== stand-in image (koinon-fakemodel) for koinon-fixtures"
  img=$(cd "$root" && KO_DOCKER_REPO=kind.local/rt1-fakemodel KIND_CLUSTER_NAME="$CLUSTER" \
    ko build --bare --platform "linux/$(go env GOARCH)" --tags escape ./cmd/koinon-fakemodel 2>"$tmp/ko.log") ||
    { cat "$tmp/ko.log" >&2; exit 1; }
  "${K[@]}" create namespace koinon-fixtures --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
fi

if (( need_gateway )); then
  echo "== gateway stand-in (koinon-fakemodel -standin) in koinon-fixtures"
  sed "s|image: rt1-fakemodel:dev|image: $img|" "$root/test/fixtures/rt1/manifests/escape-gateway-standin.yaml" |
    "${K[@]}" -n koinon-fixtures apply -f - >/dev/null
  "${K[@]}" -n koinon-fixtures rollout status deploy/escape-gateway-standin --timeout=180s
  export KOINON_ESCAPE_GATEWAY_HOST=escape-gateway-standin.koinon-fixtures.svc.cluster.local
fi

if (( need_gitea )); then
  echo "== Gitea stand-in (TLS, throwaway CA) in koinon-fixtures"
  gitea_host=escape-gitea-standin.koinon-fixtures.svc.cluster.local
  pki="$tmp/pki"
  mkdir -p "$pki"
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=koinon-escape-standin-ca" \
    -keyout "$pki/ca.key" -out "$pki/ca.crt" 2>/dev/null
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$gitea_host" -keyout "$pki/tls.key" -out "$pki/tls.csr" 2>/dev/null
  printf 'subjectAltName=DNS:%s\nbasicConstraints=CA:FALSE\nextendedKeyUsage=serverAuth\n' "$gitea_host" >"$pki/ext.cnf"
  openssl x509 -req -in "$pki/tls.csr" -CA "$pki/ca.crt" -CAkey "$pki/ca.key" -CAcreateserial -days 2 \
    -extfile "$pki/ext.cnf" -out "$pki/tls.crt" 2>/dev/null
  "${K[@]}" -n koinon-fixtures create secret generic escape-gitea-standin-tls \
    --from-file=tls.crt="$pki/tls.crt" --from-file=tls.key="$pki/tls.key" --dry-run=client -o yaml |
    "${K[@]}" apply -f - >/dev/null
  # The same secret is replaced on every run, so a running pod would keep an old certificate.
  "${K[@]}" -n koinon-fixtures delete deploy/escape-gitea-standin --ignore-not-found >/dev/null
  sed "s|image: rt1-fakemodel:dev|image: $img|" "$root/test/fixtures/rt1/manifests/escape-gitea-standin.yaml" |
    "${K[@]}" -n koinon-fixtures apply -f - >/dev/null
  "${K[@]}" -n koinon-fixtures rollout status deploy/escape-gitea-standin --timeout=180s

  # The sandboxes' proxy verifies the upstream certificate against the image's trust store: derive
  # an image that also trusts the throwaway CA (the published probe image is left untouched).
  mkdir -p "$tmp/trust"
  cp "$pki/ca.crt" "$tmp/trust/koinon-escape-standin-ca.crt"
  printf 'FROM %s\nCOPY koinon-escape-standin-ca.crt /usr/local/share/ca-certificates/\nUSER root\nRUN update-ca-certificates\n' \
    "$IMAGE" >"$tmp/trust/Dockerfile"
  docker build -q -t koinon/spike-probe:escape-trust "$tmp/trust" >/dev/null
  IMAGE=koinon/spike-probe:escape-trust
  kind load docker-image --name "$CLUSTER" "$IMAGE"
  export KOINON_ESCAPE_GITEA_URL="https://$gitea_host/gitea"
fi

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
# A listener that is not our port-forward (something else on $PORT) must not pass for it.
kill -0 "$pf" 2>/dev/null || { echo "port-forward to svc/openshell failed (is $PORT in use?):" >&2; cat "$tmp/pf.log" >&2; exit 1; }

echo "== escape suite, OpenShell cases"
cd "$root"
KOINON_OPENSHELL_ENDPOINT="127.0.0.1:$PORT" \
KOINON_OPENSHELL_MTLS_DIR="$tmp" \
KOINON_OPENSHELL_IMAGE="$IMAGE" \
  go test -count=1 -v -timeout 20m ./test/escape/ -ginkgo.label-filter=openshell
