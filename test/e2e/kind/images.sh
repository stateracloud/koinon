#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The images the kind e2e and the restore drill need, built or pulled and loaded into the kind node:
# Koinon's api, reconciler and agentd and the fake model server, fake GitHub and fake MCP server with
# ko, and the OpenCode flavor with buildx (or pulled from FLAVOR_PULL_REPO). test/e2e/run.sh and
# test/drill/run.sh call it after kind/up.sh.
#
#   CLUSTER=koinon-e1  FLAVOR_CACHE=<dir>  FLAVOR_PULL_REPO=<repo>  FLAVOR_BASE=<ref>@sha256:<digest>
#   KO=<ko binary>  GITHUB_TOKEN (authenticates the flavor build's downloads)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

CLUSTER=${CLUSTER:-koinon-e1}
root="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "$root"
ts=$(date +%s)
step() { echo "== $(date -u +%H:%M:%SZ) $*"; ts=$(date +%s); }

arch=$(go env GOARCH)
step "Koinon images (ko, linux/$arch) into kind"
# The chart's images are <image.registry>/api, /reconciler and /agentd (#167), so each is built --bare
# into its own repository; the fakes keep ko's import-path names.
for pair in api:./cmd/koinon-api reconciler:./cmd/koinon-reconciler agentd:./cmd/koinon-agentd; do
  KO_DOCKER_REPO="kind.local/${pair%%:*}" KIND_CLUSTER_NAME="$CLUSTER" "${KO:-ko}" build --bare --tags e2e \
    --platform "linux/$arch" "${pair#*:}"
done
KO_DOCKER_REPO=kind.local KIND_CLUSTER_NAME="$CLUSTER" "${KO:-ko}" build --base-import-paths --tags e2e \
  --platform "linux/$arch" ./cmd/koinon-fakemodel ./cmd/koinon-fakegithub ./cmd/koinon-fakemcp
echo "-- images: ko build and load (6 binaries): $(( $(date +%s) - ts ))s"

step "OpenCode flavor into kind"
# The flavor image is a pure function of flavors/ (hack/flavor-tag.sh). When CI's flavors job has
# pushed that tag to FLAVOR_PULL_REPO (GHCR, on main), pull it instead of building: the build is
# minutes. Anything missing falls back to the build.
flavor_tag=""
if [[ -n ${FLAVOR_PULL_REPO:-} ]]; then
  flavor_tag=$("$root/hack/flavor-tag.sh" opencode) || {
    echo "flavor tag computation failed; building the flavor image instead" >&2
    flavor_tag=""
  }
fi
pulled=0
if [[ -n $flavor_tag ]]; then
  echo "pulling flavor image $FLAVOR_PULL_REPO:$flavor_tag"
  if pull_err=$(docker pull -q "$FLAVOR_PULL_REPO:$flavor_tag" 2>&1 >/dev/null); then
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
kind load docker-image --name "$CLUSTER" kind.local/flavors/opencode:e2e
echo "-- OpenCode flavor image: $(( $(date +%s) - ts ))s"
