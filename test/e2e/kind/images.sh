#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The images the kind e2e and the restore drill need, built or pulled and loaded into the kind node:
# Koinon's api, reconciler and agentd and the fake model server, fake GitHub and fake MCP server with
# ko, and the OpenCode flavor with buildx (or pulled from FLAVOR_PULL_REPO). test/e2e/run.sh and
# test/drill/run.sh call it after kind/up.sh.
#
#   images.sh [build|load|all]   (default all)
#     build  ko builds into the local Docker daemon (retagged kind.local/...) and the flavor image is pulled
#            or built into it. Needs no cluster, so test/e2e/run.sh runs it while kind/up.sh creates one (#572).
#     load   kind load docker-image of the images `build` made. Needs the cluster.
#     all    build, then load.
#
#   CLUSTER=koinon-e1  FLAVOR_CACHE=<dir>  FLAVOR_PULL_REPO=<repo>  FLAVOR_BASE=<ref>@sha256:<digest>
#   KO=<ko binary>  GITHUB_TOKEN (authenticates the flavor build's downloads)
#   A flavor image that FLAVOR_PULL_REPO:<tag> already has in the local daemon is not pulled again (the workflow
#   pulls it first to decide whether it must set up buildx at all).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

mode=${1:-all}
case $mode in build | load | all) ;; *) echo "usage: images.sh [build|load|all]" >&2; exit 2 ;; esac

CLUSTER=${CLUSTER:-koinon-e1}
root="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "$root"
ts=$(date +%s)
step() { echo "== $(date -u +%H:%M:%SZ) $*"; ts=$(date +%s); }

# Every image the cluster needs, by the name the chart and the fakes use; `load` loads exactly these.
images=(kind.local/api:e2e kind.local/reconciler:e2e kind.local/agentd:e2e
  kind.local/koinon-fakemodel:e2e kind.local/koinon-fakegithub:e2e kind.local/koinon-fakemcp:e2e
  kind.local/flavors/opencode:e2e)

build_ko() {
  local arch pair
  arch=$(go env GOARCH)
  step "Koinon images (ko, linux/$arch) into the local Docker daemon"
  # ko.local is ko's publisher for the local daemon: it needs no cluster, so this runs while kind is created.
  # The images are retagged kind.local/... (the names the chart uses) and loaded into the node by `load`.
  # The chart's images are <image.registry>/api, /reconciler and /agentd (#167), so each is built --bare
  # into its own repository; the fakes keep ko's import-path names.
  for pair in api:./cmd/koinon-api reconciler:./cmd/koinon-reconciler agentd:./cmd/koinon-agentd; do
    KO_DOCKER_REPO="ko.local/${pair%%:*}" "${KO:-ko}" build --bare --tags e2e --platform "linux/$arch" "${pair#*:}"
    docker tag "ko.local/${pair%%:*}:e2e" "kind.local/${pair%%:*}:e2e"
  done
  KO_DOCKER_REPO=ko.local "${KO:-ko}" build --base-import-paths --tags e2e \
    --platform "linux/$arch" ./cmd/koinon-fakemodel ./cmd/koinon-fakegithub ./cmd/koinon-fakemcp
  for pair in koinon-fakemodel koinon-fakegithub koinon-fakemcp; do
    docker tag "ko.local/$pair:e2e" "kind.local/$pair:e2e"
  done
  echo "-- images: ko build (6 binaries): $(( $(date +%s) - ts ))s"
}

build_flavor() {
  step "OpenCode flavor into the local Docker daemon"
  # The flavor image is a pure function of flavors/ (hack/flavor-tag.sh). When CI's flavors job has
  # pushed that tag to FLAVOR_PULL_REPO (GHCR, on main), pull it instead of building: the build is
  # minutes. Anything missing falls back to the build.
  local flavor_tag="" pulled=0 pull_err secret cache base_out base_args
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
    docker tag "$FLAVOR_PULL_REPO:$flavor_tag" kind.local/flavors/opencode:e2e
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
      --tag kind.local/flavors/opencode:e2e flavors/opencode
  fi
  echo "-- OpenCode flavor image: $(( $(date +%s) - ts ))s"
}

load() {
  step "images into kind"
  kind load docker-image --name "$CLUSTER" "${images[@]}"
  echo "-- images: kind load (${#images[@]} images): $(( $(date +%s) - ts ))s"
}

if [[ $mode == build || $mode == all ]]; then
  build_ko
  build_flavor
fi
if [[ $mode == load || $mode == all ]]; then load; fi
