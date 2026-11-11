#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The images the kind e2e and the restore drill need, built or pulled and pushed to the local registry
# (registry.sh, localhost:5001, #45; the node pulls from it, nothing is `kind load`ed):
# Koinon's api, reconciler and agentd and the fake model server, fake GitHub and fake MCP server with
# ko, and the OpenCode flavor with buildx (or pulled from FLAVOR_PULL_REPO). test/e2e/run.sh and
# test/drill/run.sh call it.
#
#   images.sh [build|load|all]   (default all)
#     build  ko builds and pushes to the registry, and the flavor image is pulled or built and pushed. Needs the
#            registry (registry.sh start) and no cluster, so test/e2e/run.sh runs it while kind/up.sh creates
#            one (#572).
#     load   nothing left to do: the nodes pull what `build` pushed. Kept so callers need not know.
#     all    build, then load.
#
#   KIND_REGISTRY=localhost:5001  CLUSTER=koinon-e1 (images go to <registry>/<cluster>/..., imagerepo.sh)
#   FLAVOR_CACHE=<dir>  FLAVOR_PULL_REPO=<repo>  FLAVOR_BASE=<ref>@sha256:<digest>
#   KO=<ko binary>  GITHUB_TOKEN (authenticates the flavor build's downloads)
#   A flavor image that FLAVOR_PULL_REPO:<tag> already has in the local daemon is not pulled again (the workflow
#   pulls it first to decide whether it must set up buildx at all).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

mode=${1:-all}
case $mode in build | load | all) ;; *) echo "usage: images.sh [build|load|all]" >&2; exit 2 ;; esac

REGISTRY=${KIND_REGISTRY:-localhost:5001}
root="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "$root"
# <registry>/<cluster> (imagerepo.sh): the registry is shared by every kind cluster on this Docker host.
REPO=$("$root/test/e2e/kind/imagerepo.sh")
ts=$(date +%s)
step() { echo "== $(date -u +%H:%M:%SZ) $*"; ts=$(date +%s); }

build_ko() {
  local arch pair
  arch=$(go env GOARCH)
  step "Koinon images (ko, linux/$arch) pushed to $REPO"
  # The chart's images are <image.registry>/api, /reconciler and /agentd (#167), so each is built --bare
  # into its own repository; the fakes keep ko's import-path names.
  for pair in api:./cmd/koinon-api reconciler:./cmd/koinon-reconciler agentd:./cmd/koinon-agentd; do
    KO_DOCKER_REPO="$REPO/${pair%%:*}" "${KO:-ko}" build --bare --tags e2e \
      --platform "linux/$arch" "${pair#*:}"
  done
  KO_DOCKER_REPO=$REPO "${KO:-ko}" build --base-import-paths --tags e2e \
    --platform "linux/$arch" ./cmd/koinon-fakemodel ./cmd/koinon-fakegithub ./cmd/koinon-fakemcp
  echo "-- images: ko build and push (6 binaries): $(( $(date +%s) - ts ))s"
}

build_flavor() {
  step "OpenCode flavor pushed to $REPO"
  # The flavor image is a pure function of flavors/ (hack/flavor-tag.sh). When CI's flavors job has
  # pushed that tag to FLAVOR_PULL_REPO (GHCR, on main), pull it instead of building: the build is
  # minutes. Anything missing falls back to the build.
  local flavor_tag="" pulled=0 pull_err secret cache base_out base_args push_start
  if [[ -n ${FLAVOR_PULL_REPO:-} ]]; then
    flavor_tag=$("$root/hack/flavor-tag.sh" opencode) || {
      echo "flavor tag computation failed; building the flavor image instead" >&2
      flavor_tag=""
    }
  fi
  if [[ -n $flavor_tag ]]; then
    if docker image inspect "$FLAVOR_PULL_REPO:$flavor_tag" >/dev/null 2>&1; then
      echo "flavor image $FLAVOR_PULL_REPO:$flavor_tag is already here"
      pulled=1
    elif echo "pulling flavor image $FLAVOR_PULL_REPO:$flavor_tag" && pull_err=$(docker pull -q "$FLAVOR_PULL_REPO:$flavor_tag" 2>&1 >/dev/null); then
      pulled=1
    else
      echo "WARNING: pull of $FLAVOR_PULL_REPO:$flavor_tag failed, building the flavor image instead: ${pull_err:-no output}" >&2
    fi
  fi
  if [[ $pulled == 1 ]]; then
    echo "flavor image $FLAVOR_PULL_REPO:$flavor_tag pulled"
    docker tag "$FLAVOR_PULL_REPO:$flavor_tag" "$REPO/flavors/opencode:e2e"
  else
    # GITHUB_TOKEN (CI) authenticates the Dockerfile's GitHub release downloads as a BuildKit secret (#145).
    secret=()
    [[ -z ${GITHUB_TOKEN:-} ]] || secret=(--secret "id=github_token,env=GITHUB_TOKEN")
    # FLAVOR_CACHE=<dir> imports the layer cache CI restored there (import only; main's flavors job saves it).
    cache=()
    [[ -z ${FLAVOR_CACHE:-} || ! -d $FLAVOR_CACHE/opencode ]] || cache=(--cache-from "type=local,src=$FLAVOR_CACHE/opencode")
    # FLAVOR_BASE=<ref>@sha256:<digest> sets the base image (#200: a maintainer's Kata cluster builds through
    # its registry mirror); hack/flavor-base.sh checks the digest pin and prints nothing when it is unset.
    base_out=$("$root/hack/flavor-base.sh" opencode)
    base_args=()
    [[ -z $base_out ]] || mapfile -t base_args <<<"$base_out"
    docker buildx build --load --build-context common=flavors/common "${secret[@]}" "${cache[@]}" "${base_args[@]}" \
      --tag "$REPO/flavors/opencode:e2e" flavors/opencode
  fi
  # Only the layers the registry lacks are sent; on a warm registry this is a few requests.
  push_start=$(date +%s)
  docker push -q "$REPO/flavors/opencode:e2e" >/dev/null
  echo "-- OpenCode flavor push: $(( $(date +%s) - push_start ))s"
  echo "-- OpenCode flavor image: $(( $(date +%s) - ts ))s"
}

if [[ $mode == build || $mode == all ]]; then
  curl -fsS --max-time 5 "http://${REGISTRY}/v2/" >/dev/null ||
    { echo "no registry answers on $REGISTRY; run test/e2e/kind/registry.sh start (kind/up.sh does)" >&2; exit 1; }
  build_ko
  build_flavor
fi
# `load` has nothing to do: the images are in the registry and the nodes pull them (hosts.toml, registry.sh attach).
if [[ $mode == load ]]; then echo "images are in $REPO; the nodes pull them (nothing to kind load)"; fi
