#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The local registries of the kind e2e and of `make dev-kind-up` (#45). They replace `kind load`,
# which copies every image into every node on every run, and cache the third-party images the
# install pulls from the internet:
#
#   - kind-registry: the push target. Koinon's own images (ko, the OpenCode harness, the fakes) are
#     pushed to localhost:5001 and the node pulls them from it, so a layer the registry has is not
#     sent again. 127.0.0.1:5001 on the host; http://kind-registry:5000 on the kind network.
#   - kind-proxy-<host>: one pull-through cache (distribution `proxy`) per upstream host the install
#     pulls from (KIND_PROXY_HOSTS: ghcr.io and registry.k8s.io). They are wired in by the nodes'
#     containerd (/etc/containerd/certs.d/<host>/hosts.toml), which does not rewrite the reference:
#     every image name and digest is the one production uses (ADR 0061 decision 5), so no chart
#     value and no admission policy list changes. A dead cache costs speed only, containerd falls
#     back to the upstream. No credentials: every image is public, and the caches publish no port.
#
# None of them authenticates, and the push registry accepts pushes: anything on the kind network can
# overwrite an image, so this is for disposable dev and CI clusters only. Pods are on that network too
# (the nodes are). A sandbox cannot reach them: OpenShell refuses the connect, which the escape suite
# asserts by name and by address ("cannot reach the kind local registry or its caches", test/escape,
# KOINON_ESCAPE_REGISTRIES from test/escape/kind.sh); the platform's own pods can.
#
# containerd v2 reads certs.d by default (kind-config.yaml names it anyway); never the legacy
# `registry.mirrors` patch, which turns that default off. The ConfigMap
# kube-public/local-registry-hosting (KEP-1755) tells tools such as Tilt where to push.
#
#   registry.sh start            create or start the registry and the caches and join them to the kind network, creating
#                                it as kind would when missing (idempotent; no cluster needed; #956)
#   registry.sh attach <cluster> join the cluster's kind network, write the nodes' hosts.toml files and the
#                                ConfigMap (idempotent); the kube context is kind-<cluster>
#   registry.sh stop [--force]   remove the containers; the cached blobs stay (KIND_PROXY_PURGE=1 also removes the volumes).
#                                Every kind cluster on the host shares them, so stop refuses while one exists
#                                (another lane's run may be using them); --force stops them anyway
#
#   KIND_REGISTRY_PORT=5001  KIND_PROXY_HOSTS="ghcr.io registry.k8s.io"
#   KIND_PROXY_CACHE=<dir>    keep each cache's storage in <dir>/<host> (CI restores and saves it) instead
#                             of a Docker volume
#
# Every docker, kind and kubectl call names its target; no current context is used.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

# registry 3.1.2 (a multi-arch index), pinned by digest like every image (ADR 0079 checks its age).
REGISTRY_IMAGE=docker.io/library/registry:3@sha256:38907510915c3f1604c7e83d6ab602ab3020d092f73bdaed8696773693ff768c

REG_NAME=kind-registry
REG_PORT=${KIND_REGISTRY_PORT:-5001}
KIND_NET=kind
PROXY_HOSTS=${KIND_PROXY_HOSTS-"ghcr.io registry.k8s.io"}
PROXY_CACHE=${KIND_PROXY_CACHE:-}

# proxy_name <host>: the container (and DNS) name of the cache for an upstream host.
proxy_name() { echo "kind-proxy-${1//./-}"; }
# proxy_remote <host>: Docker Hub's API host is not its name.
proxy_remote() { case "$1" in docker.io) echo https://registry-1.docker.io ;; *) echo "https://$1" ;; esac; }
# all_names: the push registry and every cache.
all_names() {
  local h
  echo "$REG_NAME"
  for h in $PROXY_HOSTS; do proxy_name "$h"; done
}

# ensure <name> <publish or ""> <storage: volume name or absolute dir> [VAR=value...]: the container runs.
ensure() {
  local name=$1 publish=$2 storage=$3
  shift 3
  local state
  state=$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null || true)
  case "$state" in
  true) return 0 ;;
  false) docker start "$name" >/dev/null; return 0 ;;
  esac
  local args=(--detach --restart=always --name "$name" -v "$storage:/var/lib/registry")
  [[ -z $publish ]] || args+=(-p "$publish")
  local e
  for e in "$@"; do args+=(-e "$e"); done
  docker run "${args[@]}" "$REGISTRY_IMAGE" >/dev/null 2>&1 && return 0
  # Two up.sh runs on one host can both find no container: the loser's docker run fails on the name.
  # It waits for the winner's container (which may still be created, not running) and starts it if needed.
  local _
  for _ in $(seq 30); do
    state=$(docker inspect -f '{{.State.Status}}' "$name" 2>/dev/null || true)
    case "$state" in
    running) return 0 ;;
    "") break ;; # no container: the run failed for another reason, the last run below reports it
    created | exited) docker start "$name" >/dev/null 2>&1 || true ;;
    esac
    sleep 1
  done
  docker run "${args[@]}" "$REGISTRY_IMAGE" >/dev/null
}

# storage_for <host>: a bind-mounted directory when KIND_PROXY_CACHE is set, else a named volume.
storage_for() {
  if [[ -n $PROXY_CACHE ]]; then
    mkdir -p "$PROXY_CACHE/$1"
    echo "$PROXY_CACHE/$1"
  else
    proxy_name "$1"
  fi
}

# on_kind_net <container>: the container is on the kind network.
on_kind_net() { docker inspect -f '{{json .NetworkSettings.Networks}}' "$1" 2>/dev/null | grep -q "\"$KIND_NET\""; }

# ensure_kind_net: the kind network exists. kind reuses a network of this name and creates it only when it is
# missing (providers/docker/network.go, ensureNetwork), with the same bridge and masquerade options and the
# default bridge's MTU. The cluster here is IPv4 only (kind-config.yaml), so no IPv6 subnet is needed.
ensure_kind_net() {
  docker network inspect "$KIND_NET" >/dev/null 2>&1 && return 0
  local mtu args=(-d=bridge -o com.docker.network.bridge.enable_ip_masquerade=true)
  mtu=$(docker network inspect bridge -f '{{index .Options "com.docker.network.driver.mtu"}}' 2>/dev/null || true)
  [[ -z $mtu || $mtu == 0 ]] || args+=(-o "com.docker.network.driver.mtu=$mtu")
  # Another lane or kind itself may create it between the check and the create.
  docker network create "${args[@]}" "$KIND_NET" >/dev/null 2>&1 || docker network inspect "$KIND_NET" >/dev/null
}

# join_kind_net <container>: the container is on the kind network.
join_kind_net() {
  on_kind_net "$1" && return 0
  # Another lane's attach may connect it between the check and the connect: "already exists" is success.
  docker network connect "$KIND_NET" "$1" 2>/dev/null || on_kind_net "$1" ||
    { echo "cannot connect $1 to the $KIND_NET network" >&2; return 1; }
}

cmd_start() {
  local t0 h c
  t0=$(date +%s)
  [[ -z $PROXY_CACHE ]] || PROXY_CACHE=$(mkdir -p "$PROXY_CACHE" && cd "$PROXY_CACHE" && pwd)
  ensure "$REG_NAME" "127.0.0.1:${REG_PORT}:5000" "$REG_NAME"
  for h in $PROXY_HOSTS; do
    ensure "$(proxy_name "$h")" "" "$(storage_for "$h")" \
      "REGISTRY_PROXY_REMOTEURL=$(proxy_remote "$h")" REGISTRY_PROXY_TTL=0
  done
  # Join the kind network now, not in attach (#956). test/e2e/run.sh pushes images while kind creates the
  # cluster, and attach ran `docker network connect` in the middle of those pushes: on a Linux Docker host that
  # re-programs the container's published port, and a push in that moment gets "connection refused" (CI run
  # 43420655094: attach began at 11:55:48.29, the PATCH of koinon-fakegithub failed at 11:55:48). A registry that
  # is on the network before the first push is not touched again; attach then finds it connected.
  ensure_kind_net
  for c in $(all_names); do join_kind_net "$c"; done
  for _ in $(seq 60); do
    if curl -fsS --max-time 2 "http://127.0.0.1:${REG_PORT}/v2/" >/dev/null 2>&1; then
      echo "-- registry: localhost:${REG_PORT} up, caches: ${PROXY_HOSTS:-none}: $(( $(date +%s) - t0 ))s"
      return 0
    fi
    sleep 0.5
  done
  echo "registry $REG_NAME did not answer on 127.0.0.1:${REG_PORT} (is the port taken?)" >&2
  docker logs --tail 20 "$REG_NAME" >&2 || true
  return 1
}

# write_hosts <node> <host> <mirror url> <server> <capabilities>
write_hosts() {
  local node=$1 host=$2 mirror=$3 server=$4 caps=$5
  local dir="/etc/containerd/certs.d/$host"
  docker exec "$node" mkdir -p "$dir"
  # containerd reads certs.d on every pull; no restart.
  printf 'server = "%s"\n\n[host."%s"]\n  capabilities = %s\n' "$server" "$mirror" "$caps" |
    docker exec -i "$node" sh -c "cat > '$dir/hosts.toml'"
}

cmd_attach() {
  local cluster=${1:?usage: registry.sh attach <cluster>}
  local ctx="kind-$cluster" t0 c h node nodes
  t0=$(date +%s)
  # Normally a no-op: start joined them. It stays for a registry started by an older script.
  for c in $(all_names); do join_kind_net "$c"; done
  mapfile -t nodes < <(kind get nodes --name "$cluster")
  [[ ${#nodes[@]} -gt 0 ]] || { echo "kind cluster $cluster has no nodes" >&2; return 1; }
  for node in "${nodes[@]}"; do
    # localhost:<port> inside a node is the node itself; the push registry answers by name.
    write_hosts "$node" "localhost:${REG_PORT}" "http://${REG_NAME}:5000" "http://${REG_NAME}:5000" '["pull", "resolve", "push"]'
    for h in $PROXY_HOSTS; do
      write_hosts "$node" "$h" "http://$(proxy_name "$h"):5000" "https://$h" '["pull", "resolve"]'
    done
  done
  kubectl --context "$ctx" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${REG_PORT}"
    help: "https://kubernetes.io/docs/concepts/containers/images/#using-a-private-registry"
EOF
  echo "-- registry attached to $cluster: $(( $(date +%s) - t0 ))s"
}

cmd_stop() {
  local names clusters
  if [[ ${1:-} != --force ]]; then
    clusters=$(kind get clusters 2>/dev/null) ||
      { echo "registry.sh stop: cannot list the kind clusters; --force stops the registries anyway" >&2; return 1; }
    # kind prints "No kind clusters found." on stderr and nothing on stdout when there are none.
    if [[ -n $clusters ]]; then
      echo "registry.sh stop: kind clusters still use the shared registries ($(tr '\n' ' ' <<<"$clusters")); delete them first or pass --force" >&2
      return 1
    fi
  fi
  mapfile -t names < <(all_names)
  docker rm -f "${names[@]}" >/dev/null 2>&1 || true
  if [[ ${KIND_PROXY_PURGE:-0} == 1 ]]; then
    docker volume rm -f "${names[@]}" >/dev/null 2>&1 || true
  fi
}

case "${1:-}" in
start) cmd_start ;;
attach) cmd_attach "${2:-}" ;;
stop) cmd_stop "${2:-}" ;;
*) echo "usage: registry.sh start | attach <cluster> | stop [--force]" >&2; exit 2 ;;
esac
