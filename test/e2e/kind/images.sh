#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The images the kind e2e and the restore drill need, built or pulled and pushed to the local registry
# (registry.sh, localhost:5001, #45; the node pulls from it, nothing is `kind load`ed):
# Koinon's api, reconciler and agentd and the fake model server, fake GitHub and fake MCP server with
# ko, and the OpenCode harness with buildx (or pulled from HARNESS_PULL_REPO). test/e2e/run.sh and
# test/drill/run.sh call it.
#
#   images.sh [build|load|all]   (default all)
#     build  ko builds and pushes to the registry, and the harness image is pulled or built and pushed. Needs the
#            registry (registry.sh start) and no cluster, so test/e2e/run.sh runs it while kind/up.sh creates
#            one (#572).
#     load   nothing left to do: the nodes pull what `build` pushed. Kept so callers need not know.
#     all    build, then load.
#
#   KIND_REGISTRY=localhost:5001  CLUSTER=koinon-e1 (images go to <registry>/<cluster>/..., imagerepo.sh)
#   HARNESS_CACHE=<dir>  HARNESS_PULL_REPO=<repo>  HARNESS_BASE=<ref>@sha256:<digest>
#   KO=<ko binary>  GITHUB_TOKEN (authenticates the harness build's downloads)
#   A harness image that HARNESS_PULL_REPO:<tag> already has in the local daemon is not pulled again (the workflow
#   pulls it first to decide whether it must set up buildx at all).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

mode=${1:-all}
case $mode in build | load | all) ;; *) echo "usage: images.sh [build|load|all]" >&2; exit 2 ;; esac

# The FLAVOR_* names are the earlier ones, read until the workflows set the new ones (#610).
HARNESS_PULL_REPO=${HARNESS_PULL_REPO:-${FLAVOR_PULL_REPO:-}}
HARNESS_CACHE=${HARNESS_CACHE:-${FLAVOR_CACHE:-}}
export HARNESS_BASE=${HARNESS_BASE:-${FLAVOR_BASE:-}}

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

build_harness() {
  step "OpenCode harness pushed to $REPO"
  # The harness image is a pure function of harnesses/ (hack/harness-tag.sh). When CI's harnesses job has
  # pushed that tag to HARNESS_PULL_REPO (GHCR, on main), pull it instead of building: the build is
  # minutes. Anything missing falls back to the build.
  local harness_tag="" pulled=0 pull_err secret cache base_out base_args push_start
  if [[ -n ${HARNESS_PULL_REPO:-} ]]; then
    harness_tag=$("$root/hack/harness-tag.sh" opencode) || {
      echo "harness tag computation failed; building the harness image instead" >&2
      harness_tag=""
    }
  fi
  if [[ -n $harness_tag ]]; then
    if docker image inspect "$HARNESS_PULL_REPO:$harness_tag" >/dev/null 2>&1; then
      echo "harness image $HARNESS_PULL_REPO:$harness_tag is already here"
      pulled=1
    elif echo "pulling harness image $HARNESS_PULL_REPO:$harness_tag" && pull_err=$(docker pull -q "$HARNESS_PULL_REPO:$harness_tag" 2>&1 >/dev/null); then
      pulled=1
    else
      echo "WARNING: pull of $HARNESS_PULL_REPO:$harness_tag failed, building the harness image instead: ${pull_err:-no output}" >&2
    fi
  fi
  if [[ $pulled == 1 ]]; then
    echo "harness image $HARNESS_PULL_REPO:$harness_tag pulled"
    docker tag "$HARNESS_PULL_REPO:$harness_tag" "$REPO/sandbox:e2e"
  else
    # GITHUB_TOKEN (CI) authenticates the Dockerfile's GitHub release downloads as a BuildKit secret (#145).
    secret=()
    [[ -z ${GITHUB_TOKEN:-} ]] || secret=(--secret "id=github_token,env=GITHUB_TOKEN")
    # HARNESS_CACHE=<dir> imports the layer cache CI restored there (import only; main's harnesses job saves it).
    cache=()
    [[ -z ${HARNESS_CACHE:-} || ! -d $HARNESS_CACHE/opencode ]] || cache=(--cache-from "type=local,src=$HARNESS_CACHE/opencode")
    # HARNESS_BASE=<ref>@sha256:<digest> sets the base image (#200: a maintainer's Kata cluster builds through
    # its registry mirror); hack/harness-base.sh checks the digest pin and prints nothing when it is unset.
    base_out=$("$root/hack/harness-base.sh" opencode)
    base_args=()
    [[ -z $base_out ]] || mapfile -t base_args <<<"$base_out"
    docker buildx build --load --build-context common=harnesses/common "${secret[@]}" "${cache[@]}" "${base_args[@]}" \
      --tag "$REPO/sandbox:e2e" harnesses/opencode
  fi
  # Only the layers the registry lacks are sent; on a warm registry this is a few requests.
  push_start=$(date +%s)
  docker push -q "$REPO/sandbox:e2e" >/dev/null
  echo "-- OpenCode harness push: $(( $(date +%s) - push_start ))s"
  echo "-- OpenCode harness image: $(( $(date +%s) - ts ))s"
}

if [[ $mode == build || $mode == all ]]; then
  curl -fsS --max-time 5 "http://${REGISTRY}/v2/" >/dev/null ||
    { echo "no registry answers on $REGISTRY; run test/e2e/kind/registry.sh start (kind/up.sh does)" >&2; exit 1; }
  build_ko
  build_harness
fi
# `load` has nothing to do: the images are in the registry and the nodes pull them (hosts.toml, registry.sh attach).
if [[ $mode == load ]]; then echo "images are in $REPO; the nodes pull them (nothing to kind load)"; fi
