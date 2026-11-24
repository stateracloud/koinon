#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Proof for issue #1172 (ADR 0120 amendment 10): a pod behind the Substrate fence reaches the Kubernetes API server
# through the chart's ports-only egress rule, with NO address listed, and cannot reach another port.
#
# It makes its own throwaway kind cluster (default CNI, kindnet, which enforces NetworkPolicy on kind v0.33),
# renders the chart's fence policies with `helm template`, applies them, and probes from long-lived pods:
#   1. an unfenced pod reaches a server pod on 6443 and on 8080 (the control, so the closed port below is the policy);
#   2. the fenced pod (the label the podcert policy selects) cannot reach the server on 8080;
#   3. the fenced pod reaches the server on 6443: the rule names ports and no address, so any address works;
#   4. the fenced pod reaches the real API server endpoint on its port;
#   5. the rendered policies carry no ipBlock.
# No cluster but its own is touched: every kubectl and helm call takes the kubeconfig file of this cluster.
# Needs docker, kind, kubectl and helm 4. KEEP_CLUSTER=1 keeps the cluster for debugging.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -Eeuo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
CLUSTER=${CLUSTER:-koinon-apiegress}
NODE_IMAGE=$(sed -n 's/^NODE_IMAGE=//p' "$here/up.sh")
PROBE_IMAGE=postgres:18-alpine # busybox nc; already pulled by the test suites
tmp=$(mktemp -d "${TMPDIR:-/tmp}/apiegress.XXXXXX")
export KUBECONFIG="$tmp/kubeconfig"
k() { kubectl --kubeconfig "$KUBECONFIG" "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

cleanup() {
	if [[ ${KEEP_CLUSTER:-0} != 1 ]]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
	rm -rf "$tmp"
}
trap cleanup EXIT

kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --kubeconfig "$KUBECONFIG" --wait 180s
docker image inspect "$PROBE_IMAGE" >/dev/null 2>&1 || docker pull "$PROBE_IMAGE"
kind load docker-image "$PROBE_IMAGE" --name "$CLUSTER"

# The API server endpoint and the kubelet of the same node.
ep=$(k get endpoints kubernetes -n default -o jsonpath='{.subsets[0].addresses[0].ip}')
port=$(k get endpoints kubernetes -n default -o jsonpath='{.subsets[0].ports[0].port}')
[[ -n $ep && -n $port ]] || fail "cannot read the API server endpoint"
echo "API server endpoint $ep:$port; "

# The chart's own policies (the fence template), with the default ports and no address.
k create namespace ate-system >/dev/null
k create namespace otel-system >/dev/null
k create namespace podcertificate-controller-system >/dev/null
k create namespace unfenced >/dev/null
helm template koinon "$root/charts/koinon" --namespace koinon \
	-f "$root/charts/koinon/ci/substrate-values.yaml" \
	--show-only templates/substrate-fence-networkpolicy.yaml >"$tmp/fence.yaml"
if grep -q 'ipBlock' "$tmp/fence.yaml"; then fail "the fence must not name an address"; fi
grep -q 'port: 6443, protocol: TCP' "$tmp/fence.yaml" || fail "the fence must open the API server ports"
k apply -f "$tmp/fence.yaml" >/dev/null

# kindnet (kube-network-policies) enforces per packet, but a pod joins its pod-IP set a moment after it starts, so a
# short-lived probe pod can finish before the rules apply to it (the packets bypass). Each probe pod is therefore
# long-lived, and a check execs into it after the pod is Ready.
pod() { # pod <namespace> <name> <labels>: a Ready pod that sleeps
	local ns=$1 name=$2 labels=$3
	k run "$name" -n "$ns" --image "$PROBE_IMAGE" --image-pull-policy=Never --restart=Never ${labels:+--labels "$labels"} \
		--command -- sleep 3600 >/dev/null
	k wait -n "$ns" --for=condition=Ready "pod/$name" --timeout=90s >/dev/null
}
nc_rc() { # nc_rc <namespace> <pod> <ip> <port>: exit code of a 3-second TCP connect (0 open)
	k exec -n "$1" "$2" -- sh -c "nc -z -w 3 $3 $4 >/dev/null 2>&1; echo \$?" | tail -1
}

# A server pod outside the fence listens on 6443 and on 8080. Traffic to a node address does not pass the forward
# path that kindnet filters, so another pod is the honest target for the "any address, only these ports" claim.
k run server -n unfenced --image "$PROBE_IMAGE" --image-pull-policy=Never --restart=Never \
	--command -- sh -c 'nc -lk -p 6443 -e true & nc -lk -p 8080 -e true & wait' >/dev/null
k wait -n unfenced --for=condition=Ready pod/server --timeout=90s >/dev/null
sip=$(k get pod server -n unfenced -o jsonpath='{.status.podIP}')
fns=podcertificate-controller-system
pod "$fns" fenced "app=podcertificate-controller"
pod unfenced control ""

[[ $(nc_rc unfenced control "$sip" 6443) == 0 ]] || fail "the control: an unfenced pod must reach the server on 6443"
[[ $(nc_rc unfenced control "$sip" 8080) == 0 ]] || fail "the control: an unfenced pod must reach the server on 8080, else the checks below prove nothing"
echo "ok: control, an unfenced pod reaches both ports of the server"

# Wait until the fence applies to the pod: port 8080 closes (up to about a minute). Then the checks must hold at once.
applied=0
for _ in $(seq 1 12); do
	if [[ $(nc_rc "$fns" fenced "$sip" 8080) != 0 ]]; then applied=1; break; fi
done
[[ $applied == 1 ]] || fail "the fenced pod can still reach port 8080: the fence does not apply (does the CNI enforce NetworkPolicy?)"
echo "ok: the fenced pod cannot reach another port (8080)"
[[ $(nc_rc "$fns" fenced "$sip" 6443) == 0 ]] || fail "the fenced pod must reach TCP 6443 at any address (the rule names ports, not addresses)"
echo "ok: the fenced pod reaches TCP 6443 at an address that is not the API server"
[[ $(nc_rc "$fns" fenced "$ep" "$port") == 0 ]] || fail "the fenced pod must reach the API server endpoint on $port"
echo "ok: the fenced pod reaches the API server on $port with no address in the policy"
[[ $(nc_rc "$fns" fenced "$sip" 8080) != 0 ]] || fail "the fenced pod reached port 8080 on a second probe"
echo "PASS"
