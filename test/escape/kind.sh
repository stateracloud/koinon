#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The escape suite's OpenShell cases (ADR 0025) against the OpenShell gateway that
# `make dev-kind-up` installs on kind-koinon-dev (or DEV_KIND_CLUSTER). No registry, no home lab:
# the probe image is built from hack/spike/probe (or, when KOINON_OPENSHELL_IMAGE names an image that
# pulls (it is no longer published; #167), pulled instead: no
# nvcr.io or apt at run time, old issue 357) and loaded into the node. PROBE_PULL_REPO=<repo> pulls
# <repo>:<hack/probe-tag.sh tag> (the image ci.yml's flavor-publish pushes on main) when
# KOINON_OPENSHELL_IMAGE is unset, so the e2e needs no apt mirror (#482); a miss falls back to the build; the gateway is reached through a
# port-forward with the client certificate it generated.
#
# The gateway-route case needs a stand-in for agentgateway on port 4000 (ADR 0080, old issue 270): unless
# KOINON_ESCAPE_GATEWAY_HOST already names one, or KOINON_ESCAPE_STANDIN=0, it is built from
# cmd/koinon-fakemodel with ko and run in namespace koinon-fixtures, as the RT1 fixtures do.
#
# The Gitea read-grant specs need a TLS server behind the host their grant names, because the proxy
# dials an allowed host before it judges the request (old issue 371). Unless KOINON_ESCAPE_GITEA_URL is set,
# or KOINON_ESCAPE_STANDIN=0, a stand-in (koinon-fakemodel -standin over TLS, in koinon-fixtures)
# gets a certificate from a throwaway CA. OpenShell 0.1.7's supervisor (its own container, not the
# sandbox image) verifies upstream TLS against webpki roots plus an extra CA bundle, so the CA goes into a
# ConfigMap named by supervisor.network.additionalCaConfigMapName; the supervisor image is the stock one.
# Nothing leaves the cluster.
#
# The leak guard exfiltration case (exfil_test.go, #293) needs a guarded gateway this script does not
# start: it runs when KOINON_ESCAPE_MCP_HOST, KOINON_ESCAPE_MCP_KEY and KOINON_ESCAPE_MCP_RECEIVER_URL are
# set in the environment (docs/dev/ci.md#leak-guard-exfiltration-cases) and skips otherwise. `make dev-kind-up`
# installs OpenShell only, so from there it skips. The kind e2e (test/e2e/run.sh with ESCAPE_MCP=1) calls this
# script from its step 6b through test/e2e/escape-mcp.sh, against the quickstart install, with
# KOINON_ESCAPE_LABEL=leakguard, KOINON_ESCAPE_STANDIN=0 and KOINON_REQUIRE=escape-mcp-gateway (#348).
# KOINON_ESCAPE_LABEL is the Ginkgo label filter (default openshell).
#
# Against the chart install (the kind e2e, test/e2e/escape-openshell.sh, #109): KOINON_ESCAPE_CHART_NAMESPACE
# names the Koinon release namespace. The script then points the suite's live admission case and the
# NetworkPolicy bypass case at the workspace namespace and the chart's policy (KOINON_ESCAPE_NAMESPACE,
# _POLICY, _NETPOL, _SUPERVISOR_IMAGE, KOINON_ESCAPE_POLICY_REQUIRED=1), so the real
# ValidatingAdmissionPolicy is enforced on the real supervisor and workload pods and the case fails,
# not skips, when it is not. Bare `make test-escape-kind` leaves those unset (kindnet does not
# enforce NetworkPolicy). KOINON_ESCAPE_GITEA_STANDIN=0 leaves the Gitea stand-in out: it
# re-points the gateway of the OpenShell install at a CA ConfigMap by helm upgrade, which would drift the
# chart install; the Gitea cases run in `make test-escape-kind`.
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
# Live admission, NetworkPolicy bypass and realPods assume the chart install (and, for NetworkPolicy,
# kube-network-policies from test/e2e/kind/up.sh). Bare `make test-escape-kind` must not export
# KOINON_ESCAPE_NAMESPACE: kindnet does not enforce NetworkPolicy, and the live specs would run
# against whatever supervisor image OpenShell actually uses.
if [[ -n ${KOINON_ESCAPE_CHART_NAMESPACE:-} ]]; then
  # OpenShell names a managed workspace's namespace openshell-<gateway id>-<workspace> (the gateway ID is
  # the chart's openshell.gatewayId, default openshell; internal/execution.WorkspaceNamespace).
  export KOINON_ESCAPE_NAMESPACE="openshell-openshell-${KOINON_OPENSHELL_WORKSPACE:-koinon-escape}"
  export KOINON_ESCAPE_POLICY="${KOINON_ESCAPE_CHART_NAMESPACE}-koinon-sandbox-pods"
  export KOINON_ESCAPE_NETPOL="${KOINON_ESCAPE_CHART_NAMESPACE}-koinon-sandbox-default-deny"
  export KOINON_ESCAPE_LIMITRANGE="${KOINON_ESCAPE_CHART_NAMESPACE}-koinon-sandbox-limitrange"
  export KOINON_ESCAPE_POLICY_REQUIRED=1
  # The class the install's policy admits for workload pods: openshell.runtimeClassName of
  # test/e2e/kind/koinon-values.yaml (runc on kind), which sandboxPolicy.runtimeClasses lists.
  KOINON_ESCAPE_RUNTIME_CLASS=${KOINON_ESCAPE_RUNTIME_CLASS:-$(sed -n 's/^  runtimeClassName: *//p' "$root/test/e2e/kind/koinon-values.yaml" | head -1)}
  export KOINON_ESCAPE_RUNTIME_CLASS=${KOINON_ESCAPE_RUNTIME_CLASS:-runc}
  export KOINON_ESCAPE_SERVICES=${KOINON_ESCAPE_SERVICES:-openshell/openshell:8080,${KOINON_ESCAPE_CHART_NAMESPACE}/koinon-gateway:8080,${KOINON_ESCAPE_CHART_NAMESPACE}/koinon-api:8080}
  KOINON_ESCAPE_SUPERVISOR_IMAGE="ghcr.io/nvidia/openshell/supervisor:$(sed -n 's/^OS_VERSION=//p' "$root/test/e2e/kind/up.sh")"
  export KOINON_ESCAPE_SUPERVISOR_IMAGE
fi

kind get clusters 2>/dev/null | grep -qx "$CLUSTER" || { echo "no kind cluster $CLUSTER: run 'make dev-kind-up' first" >&2; exit 1; }
K=(kubectl --context "$CTX")
"${K[@]}" -n openshell get secret openshell-client-tls >/dev/null ||
  { echo "the OpenShell gateway is not installed in $CTX (make dev-kind-up installs it)" >&2; exit 1; }

# The cluster's own Service IPs for the deny checks (they differ per cluster; the suite has no default).
# Values the caller exported win.
if [[ -z ${KOINON_ESCAPE_CLUSTER_DNS:-} ]]; then
  KOINON_ESCAPE_CLUSTER_DNS=$("${K[@]}" -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}') ||
    { echo "cannot read the kube-dns Service ClusterIP from $CTX" >&2; exit 1; }
fi
api_ip=$("${K[@]}" -n default get svc kubernetes -o jsonpath='{.spec.clusterIP}') ||
  { echo "cannot read the kubernetes Service ClusterIP from $CTX" >&2; exit 1; }
[[ -n $KOINON_ESCAPE_CLUSTER_DNS && -n $api_ip ]] || { echo "empty kube-dns or kubernetes ClusterIP from $CTX" >&2; exit 1; }
# This script always provides the vars, so the deny specs that read them must fail, not skip, if they are lost.
export KOINON_REQUIRE="${KOINON_REQUIRE:+$KOINON_REQUIRE,}escape-cluster-ips"
export KOINON_ESCAPE_CLUSTER_DNS
export KOINON_ESCAPE_API=${KOINON_ESCAPE_API:-${api_ip}:443,kubernetes.default.svc.cluster.local:443,kubernetes.default.svc:443}
export KOINON_ESCAPE_INTERNAL=${KOINON_ESCAPE_INTERNAL:-${api_ip}:6443,openshell.openshell.svc.cluster.local:8080,agentgateway-mgmt.agentgateway.svc.cluster.local:4001,agentgateway.agentgateway.svc.cluster.local:8080}
# The kind local registry and its pull-through caches (test/e2e/kind/registry.sh, #45) authenticate nobody and
# the push registry accepts pushes, so a sandbox must not reach them: by container name and by address on the
# kind network. Read from Docker; a host without them (an older up.sh) leaves the case skipped.
if [[ -z ${KOINON_ESCAPE_REGISTRIES:-} ]]; then
  regs=()
  while read -r name; do
    regs+=("$name:5000")
    ip=$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "$name" 2>/dev/null || true)
    [[ -z $ip ]] || regs+=("$ip:5000")
  done < <(docker ps --filter network=kind --format '{{.Names}}' | grep -E '^kind-(registry|proxy-)' || true)
  KOINON_ESCAPE_REGISTRIES=$(IFS=,; echo "${regs[*]}")
fi
if [[ -n $KOINON_ESCAPE_REGISTRIES ]]; then
  export KOINON_ESCAPE_REGISTRIES
  KOINON_REQUIRE="$KOINON_REQUIRE,escape-kind-registry"
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/kind.XXXXXX")
pf=
diagnostics() {
  local rc=$?
  (( rc == 0 )) || {
    echo "== diagnostics (failed run)" >&2
    # The stand-in namespace exists only when this run made stand-ins; say nothing when it is not there.
    if "${K[@]}" get namespace koinon-fixtures >/dev/null 2>&1; then
      "${K[@]}" -n koinon-fixtures logs deploy/escape-gitea-standin --tail=50 >&2 || true
    fi
    if [[ -n ${KOINON_ESCAPE_CHART_NAMESPACE:-} ]]; then
      echo "-- admission refusals and the chart's policies (the install enforces sandboxPolicy)" >&2
      "${K[@]}" get events -A 2>&1 | grep -i 'ValidatingAdmissionPolicy' | tail -20 >&2 || true
      "${K[@]}" get validatingadmissionpolicies,validatingadmissionpolicybindings -l app.kubernetes.io/part-of=koinon >&2 || true
      # A policy refusal is mostly not an Event: the OpenShell gateway and the Agent Sandbox controller log it.
      echo "-- 'denied request' in the OpenShell gateway and Agent Sandbox controller logs" >&2
      for d in openshell/statefulset/openshell openshell/deploy/openshell agent-sandbox-system/deploy/agent-sandbox-controller; do
        "${K[@]}" -n "${d%%/*}" logs "${d#*/}" --tail=500 2>&1 | grep -i 'denied request' | tail -20 >&2 || true
      done
    fi
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

tagged_probe=0
if [[ -z ${KOINON_OPENSHELL_IMAGE:-} && -n ${PROBE_PULL_REPO:-} ]]; then
  if probe_tag=$("$root/hack/probe-tag.sh"); then
    KOINON_OPENSHELL_IMAGE="$PROBE_PULL_REPO:$probe_tag"
    tagged_probe=1
    IMAGE=$KOINON_OPENSHELL_IMAGE
  else
    echo "probe tag computation failed; building the probe image instead" >&2
  fi
fi
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
  # KOINON_PROBE_BASE=<image> builds on an image already on this machine. With PROBE_PULL_REPO set the image
  # carries a content tag that names the pinned base (hack/probe-tag.sh), so the fallback ignores
  # KOINON_PROBE_BASE: one tag, one image. An explicit KOINON_OPENSHELL_IMAGE keeps the override. Unset, the base is the digest pin of the OpenCode flavor
  # Dockerfile, the one definition of that base.
  (( ! tagged_probe )) || KOINON_PROBE_BASE=
  probe_base=${KOINON_PROBE_BASE:-$(sed -n 's/^ARG BASE=//p' "$root/flavors/opencode/Dockerfile" | head -n 1)}
  [[ -n $probe_base ]] || { echo "no probe base: set KOINON_PROBE_BASE or restore ARG BASE= in flavors/opencode/Dockerfile" >&2; exit 1; }
  probe_args=(--build-arg "BASE=$probe_base")
  docker build -q "${probe_args[@]}" -t "$IMAGE" "$root/hack/spike/probe" >/dev/null
fi
kind load docker-image --name "$CLUSTER" "$IMAGE"

need_gateway=0 need_gitea=0
[[ -z ${KOINON_ESCAPE_GATEWAY_HOST:-} && ${KOINON_ESCAPE_STANDIN:-1} != 0 ]] && need_gateway=1
[[ -z ${KOINON_ESCAPE_GITEA_URL:-} && ${KOINON_ESCAPE_STANDIN:-1} != 0 && ${KOINON_ESCAPE_GITEA_STANDIN:-1} != 0 ]] && need_gitea=1

if (( need_gateway || need_gitea )); then
  echo "== stand-in image (koinon-fakemodel) for koinon-fixtures"
  # A cluster with the local registry (test/e2e/kind/registry.sh, #45; its KEP-1755 ConfigMap) gets the
  # stand-in pushed to the cluster's path there, not kind-loaded. The kubelet has then pulled the same
  # digest itself (the e2e's fakemodel, the same reproducible ko build), and with
  # KubeletEnsureSecretPulledImages (beta, on since Kubernetes 1.35) a pulled image counts as verified
  # only for the names it was pulled under: a kind-loaded kind.local name for that image is pulled
  # again, which cannot work (ImagePullBackOff). Without the registry, nothing is pulled and kind.local
  # stays as before.
  stand_repo=kind.local/rt1-fakemodel
  reg_host=$("${K[@]}" -n kube-public get configmap local-registry-hosting \
    -o jsonpath='{.data.localRegistryHosting\.v1}' 2>/dev/null | sed -n 's/^host: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' || true)
  [[ -z $reg_host ]] ||
    stand_repo="$(KIND_REGISTRY="$reg_host" CLUSTER="$CLUSTER" "$root/test/e2e/kind/imagerepo.sh")/rt1-fakemodel"
  img=$(cd "$root" && KO_DOCKER_REPO="$stand_repo" KIND_CLUSTER_NAME="$CLUSTER" \
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

  # OpenShell 0.1.7's supervisor verifies the upstream certificate against webpki roots plus the extra
  # CA bundle the gateway hands it. The chart takes that bundle from an operator-owned ConfigMap with a
  # ca.crt key in the gateway namespace (supervisor.network.additionalCaConfigMapName, chart README
  # "Additional destination CAs"). The gateway reads it at startup and the chart does not checksum the
  # ConfigMap, so the name carries a digest of the CA: every run makes a new CA, a new name changes the helm
  # value, and the upgrade (--wait) rolls the gateway. The supervisor image stays the stock one.
  # The chart ref is the one test/e2e/kind/up.sh installs.
  os_version=$(sed -n 's/^OS_VERSION=//p' "$root/test/e2e/kind/up.sh")
  os_digest=$(sed -n 's/^OS_DIGEST=//p' "$root/test/e2e/kind/up.sh")
  echo "== additional destination CA (throwaway CA) for the supervisor"
  ca_sum=$(openssl x509 -in "$pki/ca.crt" -noout -fingerprint -sha256 | tr -d ':' | sed 's/.*=//' | tr 'A-F' 'a-f' | cut -c1-12)
  ca_cm="escape-destination-ca-$ca_sum"
  "${K[@]}" -n openshell create configmap "$ca_cm" \
    --from-file=ca.crt="$pki/ca.crt" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  helm --kube-context "$CTX" upgrade openshell "oci://ghcr.io/nvidia/openshell/helm-chart@${os_digest}" \
    --version "$os_version" --namespace openshell --reuse-values \
    --set supervisor.network.additionalCaConfigMapName="$ca_cm" \
    --wait --timeout 5m
  # Older CA ConfigMaps from earlier runs are no longer referenced.
  "${K[@]}" -n openshell get configmap -o name | grep '^configmap/escape-destination-ca-' |
    grep -v "/$ca_cm\$" | xargs -r "${K[@]}" -n openshell delete >/dev/null || true
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
