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
cat >"$tmp/bin/docker" <<'SH'
#!/usr/bin/env bash
echo "docker $*" >>"$STATE/calls.log"
case "$1" in
inspect)
  name=${*: -1}
  [[ -e $STATE/c-$name ]] || exit 1
  case "$*" in *Networks*) cat "$STATE/c-$name" ;; *) echo true ;; esac ;;
run)
  while (($#)); do [[ $1 == --name ]] && name=$2; shift; done
  echo '{"bridge":{}}' >"$STATE/c-$name" ;;
network)
  echo '{"bridge":{},"kind":{}}' >"$STATE/c-${*: -1}" ;;
exec)
  if [[ $2 == -i ]]; then
    file=$(sed -n "s/.*cat > '\(.*\)'.*/\1/p" <<<"$*")
    mkdir -p "$STATE/node$(dirname "$file")"
    cat >"$STATE/node$file"
  fi ;;
esac
SH
printf '#!/usr/bin/env bash\necho koinon-test-control-plane\n' >"$tmp/bin/kind"
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

"$reg" attach koinon-test >/dev/null
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

((fail == 0)) || exit 1
echo "kind-registry-test: all passed"
