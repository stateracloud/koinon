#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The escape suite's OpenShell cases (ADR 0025) against the OpenShell gateway that
# `make dev-kind-up` installs on kind-koinon-dev (or DEV_KIND_CLUSTER). No registry, no home lab:
# the probe image is built from hack/spike/probe (or, when KOINON_OPENSHELL_IMAGE names an image that
# pulls (it is no longer published; #167), pulled instead: no
# nvcr.io or apt at run time, old issue 357) and loaded into the node; the gateway is reached through a
# port-forward with the client certificate it generated.
#
# The gateway-route case needs a stand-in for agentgateway on port 4000 (ADR 0080, old issue 270): unless
# KOINON_ESCAPE_GATEWAY_HOST already names one, or KOINON_ESCAPE_STANDIN=0, it is built from
# cmd/koinon-fakemodel with ko and run in namespace koinon-fixtures, as the RT1 fixtures do.
#
# The Gitea read-grant specs need a TLS server behind the host their grant names, because the proxy
# dials an allowed host before it judges the request (old issue 371). Unless KOINON_ESCAPE_GITEA_URL is set,
# or KOINON_ESCAPE_STANDIN=0, a stand-in (koinon-fakemodel -standin over TLS, in koinon-fixtures)
# gets a certificate from a throwaway CA. OpenShell 0.1.5's supervisor (its own container and image,
# not the sandbox image) verifies upstream TLS against webpki roots plus the supervisor image's
# /etc/ssl/certs/ca-certificates.crt, so the gateway is re-pointed at a supervisor image derived
# with that CA appended. Interim until OpenShell >= 0.1.7 (supervisor.network.additionalCaConfigMapName),
# #80. Nothing leaves the cluster.
#
# The leak guard exfiltration case (exfil_test.go, #293) needs a guarded gateway this script does not
# start: it runs when KOINON_ESCAPE_MCP_HOST, KOINON_ESCAPE_MCP_KEY and KOINON_ESCAPE_MCP_RECEIVER_URL are
# set in the environment (docs/dev/ci.md#leak-guard-exfiltration-cases) and skips otherwise. `make dev-kind-up`
# installs OpenShell only, so from there it skips. The kind e2e (test/e2e/run.sh with ESCAPE_MCP=1) calls this
# script from its step 6b through test/e2e/escape-mcp.sh, against the quickstart install, with
# KOINON_ESCAPE_LABEL=leakguard, KOINON_ESCAPE_STANDIN=0 and KOINON_REQUIRE=escape-mcp-gateway (#348).
# KOINON_ESCAPE_LABEL is the Ginkgo label filter (default openshell).
#
# kind runs sandboxes under runc, so the Kata guest case skips (it needs KOINON_ESCAPE_HOST_KERNEL
# and a Kata node, the maintainer's release gate). Cases that need other fixtures skip with their
# reason; the rest run. Every kubectl call names the kind context and nothing else is touched.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

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

tmp=$(mktemp -d "${TMPDIR:-/tmp}/kind.XXXXXX")
pf=
diagnostics() {
  local rc=$?
  (( rc == 0 )) || {
    echo "== diagnostics (failed run)" >&2
    "${K[@]}" -n koinon-fixtures logs deploy/escape-gitea-standin --tail=50 >&2 || true
    echo "-- pods not Running in openshell/sandbox namespaces" >&2
    "${K[@]}" get pods -A -o wide 2>&1 | awk 'NR==1 || (($1 ~ /openshell|sandbox/) && $4 != "Running")' >&2 || true
    "${K[@]}" get pods -A --no-headers 2>/dev/null | awk '($1 ~ /openshell|sandbox/) && $4 != "Running" && $4 != "Completed" {print $1, $2}' |
      while read -r ns pod; do
        echo "-- describe $ns/$pod (events)" >&2
        "${K[@]}" -n "$ns" describe pod "$pod" 2>&1 | sed -n '/^Events:/,$p' | tail -15 >&2 || true
      done
    "${K[@]}" get pods -A --no-headers 2>/dev/null | awk '$2 ~ /^os-supervisor-/ {print $1, $2}' |
      while read -r ns pod; do
        echo "-- $ns/$pod" >&2
        "${K[@]}" -n "$ns" logs "$pod" --all-containers --tail=200 2>&1 | grep -E -i 'certificate|UnknownIssuer|upstream' >&2 || true
      done
  }
  return 0
}
trap 'diagnostics; [[ -z $pf ]] || { kill "$pf" 2>/dev/null; wait "$pf" 2>/dev/null; } || true; rm -rf "$tmp"' EXIT

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

  # OpenShell 0.1.5's supervisor verifies the upstream certificate against webpki roots plus
  # /etc/ssl/certs/ca-certificates.crt in the SUPERVISOR image (distroless, no shell), not in the
  # sandbox image. Derive a supervisor image from the exact one the install runs, with the throwaway
  # CA appended, and point the gateway at it. Interim, until OpenShell >= 0.1.7 can take an extra CA
  # (supervisor.network.additionalCaConfigMapName): #80 removes this block. The chart ref is the one
  # test/e2e/kind/up.sh installs.
  os_version=$(sed -n 's/^OS_VERSION=//p' "$root/test/e2e/kind/up.sh")
  os_digest=$(sed -n 's/^OS_DIGEST=//p' "$root/test/e2e/kind/up.sh")
  sup_src=ghcr.io/nvidia/openshell/supervisor:$os_version
  sup_reg=kind.local sup_repo=koinon/escape-supervisor sup_tag=$os_version-escape-ca
  sup_ref=$sup_reg/$sup_repo:$sup_tag # built, loaded and rendered by the chart under exactly this name
  echo "== supervisor image $sup_src + throwaway CA"
  mkdir -p "$tmp/sup"
  docker pull -q "$sup_src" >/dev/null
  cid=$(docker create "$sup_src" /bin/true)
  docker cp "$cid:/etc/ssl/certs/ca-certificates.crt" "$tmp/sup/ca-certificates.crt" || { docker rm "$cid" >/dev/null; exit 1; }
  docker rm "$cid" >/dev/null
  cat "$pki/ca.crt" >>"$tmp/sup/ca-certificates.crt"
  printf 'FROM %s\nCOPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\n' "$sup_src" >"$tmp/sup/Dockerfile"
  docker build -q -t "$sup_ref" "$tmp/sup" >/dev/null
  kind load docker-image --name "$CLUSTER" "$sup_ref"
  # supervisor.image.{registry,repository,tag,pullPolicy} are the 0.1.5 chart's keys (values.yaml);
  # the registry is set so the image is kind.local/..., never pulled from anywhere.
  helm --kube-context "$CTX" upgrade openshell "oci://ghcr.io/nvidia/openshell/helm-chart@${os_digest}" \
    --version "$os_version" --namespace openshell --reuse-values \
    --set "supervisor.image.registry=$sup_reg" --set "supervisor.image.repository=$sup_repo" \
    --set "supervisor.image.tag=$sup_tag" --set supervisor.image.pullPolicy=IfNotPresent \
    --wait --timeout 5m
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
  go test -count=1 -v -timeout 20m ./test/escape/ -ginkgo.label-filter="${KOINON_ESCAPE_LABEL:-openshell}"
