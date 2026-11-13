#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests test/e2e/kind/registry.sh (#45) with stub docker, kind, kubectl and curl: what it starts,
# what it writes into a node, that a second run changes nothing, and that it never uses the legacy
# `registry.mirrors` form or the current kube context.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."
reg=$PWD/test/e2e/kind/registry.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/kind-registry-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/state"

# docker: a container exists once `run` made it; `exec -i ... sh -c "cat > <file>"` stores stdin under state/node.
# state/race-<name> makes the next `run` of <name> lose to another lane: the container appears, created but
# not started, and the run fails on the name. state/netrace-<name> makes the next `network connect` find
# the container already connected by another lane.
cat >"$tmp/bin/docker" <<'SH'
#!/usr/bin/env bash
echo "docker $*" >>"$STATE/calls.log"
case "$1" in
inspect)
  name=${*: -1}
  [[ -e $STATE/c-$name ]] || exit 1
  status=$(cat "$STATE/s-$name" 2>/dev/null || echo running)
  case "$*" in
  *Networks*) cat "$STATE/c-$name" ;;
  *State.Status*) echo "$status" ;;
  *) [[ $status == running ]] && echo true || echo false ;;
  esac ;;
run)
  while (($#)); do [[ $1 == --name ]] && name=$2; shift; done
  echo '{"bridge":{}}' >"$STATE/c-$name"
  if [[ -e $STATE/race-$name ]]; then
    rm "$STATE/race-$name"
    echo created >"$STATE/s-$name"
    echo "Conflict. The container name \"/$name\" is already in use" >&2
    exit 125
  fi ;;
start)
  echo running >"$STATE/s-${*: -1}" ;;
network)
  # inspect <net>: the network exists once create made it; create <net>: record it.
  case "$2" in
  inspect) [[ -e $STATE/net-${*: -1} ]] || exit 1; exit 0 ;;
  create) touch "$STATE/net-${*: -1}"; exit 0 ;;
  esac
  name=${*: -1}
  echo '{"bridge":{},"kind":{}}' >"$STATE/c-$name"
  if [[ -e $STATE/netrace-$name ]]; then
    rm "$STATE/netrace-$name"
    echo "endpoint with name $name already exists in network kind" >&2
    exit 1
  fi ;;
exec)
  if [[ $2 == -i ]]; then
    file=$(sed -n "s/.*cat > '\(.*\)'.*/\1/p" <<<"$*")
    mkdir -p "$STATE/node$(dirname "$file")"
    cat >"$STATE/node$file"
  fi ;;
esac
SH
# kind: `get clusters` lists state/clusters (none when it is missing), `get nodes` one node.
cat >"$tmp/bin/kind" <<'SH'
#!/usr/bin/env bash
if [[ "$*" == "get clusters" ]]; then cat "$STATE/clusters" 2>/dev/null || true; else echo koinon-test-control-plane; fi
SH
cat >"$tmp/bin/kubectl" <<'SH'
#!/usr/bin/env bash
echo "kubectl $*" >>"$STATE/calls.log"
cat >"$STATE/configmap.yaml"
SH
printf '#!/usr/bin/env bash\nexit 0\n' >"$tmp/bin/curl"
chmod +x "$tmp/bin/"*
export STATE=$tmp/state PATH=$tmp/bin:$PATH

fail=0
check() { # check <description> <command...>
  local d=$1
  shift
  if "$@"; then echo "ok   $d"; else echo "FAIL $d" >&2; fail=1; fi
}
runs() { grep -c '^docker run ' "$STATE/calls.log" || true; }

"$reg" start >/dev/null
check "start creates the push registry and two caches" test "$(runs)" = 3
check "push registry is published on 127.0.0.1:5001 only" grep -q '^docker run .*-p 127.0.0.1:5001:5000 .*--name kind-registry\|^docker run .*--name kind-registry .*-p 127.0.0.1:5001:5000' "$STATE/calls.log"
check "caches publish no port" bash -c "! grep '^docker run .*kind-proxy-' '$STATE/calls.log' | grep -q -- ' -p '"
check "the cache for ghcr.io has the upstream and ttl 0" grep -q 'kind-proxy-ghcr-io.*REGISTRY_PROXY_REMOTEURL=https://ghcr.io.*REGISTRY_PROXY_TTL=0' "$STATE/calls.log"
check "the registry image is pinned by digest" grep -q 'registry:3@sha256:[0-9a-f]\{64\}' "$STATE/calls.log"

# #956: the containers join the kind network at start, before any push; attach has nothing to connect, because
# a `docker network connect` in the middle of a push makes the published port refuse it.
check "start creates the kind network" grep -q '^docker network create .* kind$' "$STATE/calls.log"
check "start connects the registry and the caches to the kind network" bash -c "test \$(grep -c '^docker network connect' '$STATE/calls.log') = 3"
connects=$(grep -c '^docker network connect' "$STATE/calls.log")
"$reg" attach koinon-test >/dev/null
check "attach connects no network after start" test "$(grep -c '^docker network connect' "$STATE/calls.log")" = "$connects"
node=$STATE/node/etc/containerd/certs.d
check "node trusts the push registry" grep -q 'host."http://kind-registry:5000"' "$node/localhost:5001/hosts.toml"
check "push registry may be pushed to" grep -q '"push"' "$node/localhost:5001/hosts.toml"
check "ghcr.io is mirrored by its cache and falls back to the upstream" bash -c "grep -q 'server = \"https://ghcr.io\"' '$node/ghcr.io/hosts.toml' && grep -q 'host.\"http://kind-proxy-ghcr-io:5000\"' '$node/ghcr.io/hosts.toml'"
check "registry.k8s.io is mirrored too" test -s "$node/registry.k8s.io/hosts.toml"
check "a cache is never pushed to" bash -c "! grep -q push '$node/ghcr.io/hosts.toml'"
check "the KEP-1755 ConfigMap names localhost:5001" grep -q 'host: "localhost:5001"' "$STATE/configmap.yaml"
check "kubectl names the kind context" grep -q '^kubectl --context kind-koinon-test ' "$STATE/calls.log"

before=$(runs)
"$reg" start >/dev/null
"$reg" attach koinon-test >/dev/null
check "a second start and attach create nothing" test "$(runs)" = "$before"
check "a second attach connects no network again" bash -c "test \$(grep -c '^docker network connect' '$STATE/calls.log') = 3"

check "the script never uses the legacy mirrors patch" bash -c "! grep -v '^ *#' '$reg' | grep -q 'registry.mirrors'"

# Clusters share the registry, so their images must not share a reference (a second run would
# overwrite the first run's :e2e tags).
repo=$PWD/test/e2e/kind/imagerepo.sh
a=$(CLUSTER=koinon-e1 "$repo")
b=$(CLUSTER=koinon-e2e-runner "$repo")
check "two clusters get different image repositories" test "$a" != "$b"
check "an image repository is <registry>/<cluster>" test "$a" = "localhost:5001/koinon-e1"
check "KIND_REGISTRY moves the repository" test "$(KIND_REGISTRY=r.test:1 CLUSTER=c1 "$repo")" = "r.test:1/c1"
for f in test/e2e/kind/images.sh test/e2e/run.sh test/drill/run.sh test/e2e/escape-openshell.sh; do
  check "$f takes the image repository from imagerepo.sh" grep -q 'imagerepo.sh' "$f"
done
check "images.sh pushes nothing outside the cluster's path" bash -c "! grep -v '^ *#' test/e2e/kind/images.sh | grep -q 'KO_DOCKER_REPO=\$REGISTRY\|\$REGISTRY/harnesses'"

# Two lanes at once (#600 review).
rm "$STATE/c-kind-registry"
: >"$STATE/race-kind-registry"
check "start succeeds when another lane's run wins the registry's name" "$reg" start
check "the loser starts the winner's created container" test "$(cat "$STATE/s-kind-registry")" = running

echo '{"bridge":{}}' >"$STATE/c-kind-proxy-ghcr-io"
: >"$STATE/netrace-kind-proxy-ghcr-io"
check "attach succeeds when another lane connects a cache first" bash -c "'$reg' attach koinon-test >/dev/null"

echo koinon-test >"$STATE/clusters"
: >"$STATE/calls.log"
check "stop refuses while a kind cluster exists" bash -c "! '$reg' stop 2>/dev/null"
check "a refused stop removes nothing" bash -c "! grep -q '^docker rm' '$STATE/calls.log'"
check "stop --force stops them anyway" bash -c "'$reg' stop --force && grep -q '^docker rm -f kind-registry' '$STATE/calls.log'"
rm "$STATE/clusters"
: >"$STATE/calls.log"
check "stop with no cluster left removes the registry and the caches" bash -c "'$reg' stop && grep -q '^docker rm -f kind-registry kind-proxy-ghcr-io kind-proxy-registry-k8s-io' '$STATE/calls.log'"

((fail == 0)) || exit 1
echo "kind-registry-test: all passed"
