#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds the RT1 fixture images for linux/amd64 and pushes them to a registry (see
# test/fixtures/rt1/README.md):
#   <registry>/koinon/rt1-fakecicost     ko, from cmd/koinon-fakecicost
#   <registry>/koinon/rt1-promgen        ko, from cmd/koinon-rt1-promgen
#   <registry>/koinon/rt1-motherduck     docker buildx, from test/fixtures/rt1/motherduck
#   <registry>/koinon/rt1-prometheus-mcp docker buildx, from test/fixtures/rt1/prometheus (Dockerfile.mcp)
#   <registry>/koinon/rt1-prometheus     a copy of the pinned prom/prometheus image (promtool lives in it)
#   <registry>/koinon/rt1-searxng        a copy, by digest, of searxng/searxng (AGPL-3.0, unmodified)
#   <registry>/koinon/rt1-searxng-mcp    a copy, by digest, of isokoliuk/mcp-searxng
#
# Usage: hack/rt1-fixtures-build.sh REGISTRY [TAG]     e.g. zot.apps.lab.example.invalid, TAG defaults to dev
# DRY_RUN=1 prints the commands and pushes nothing. You must be logged in to the registry (docker login
# or a DOCKER_CONFIG that has it). Prints each image's digest at the end, for kustomize.
set -euo pipefail

registry="${1:?usage: $0 REGISTRY [TAG]}"
tag="${2:-dev}"
platform=linux/amd64
prometheus=prom/prometheus:v3.7.3@sha256:49214755b6153f90a597adcbff0252cc61069f8ab69ce8411285cd4a560e8038
searxng=searxng/searxng@sha256:0f48ffe2152f388f5eaede27f96b610449cebfc890d0ad457304475a54ad7b6e
searxng_tag=2026.10.27
searxng_mcp=isokoliuk/mcp-searxng@sha256:8f1dd92a9e2875cb4e8df7036785ec4c088b7600cefb611de472a352a3dcba60
searxng_mcp_tag=2.9.0
root="$(cd "$(dirname "$0")/.." && pwd)"
repo="${registry%/}/koinon"

run() {
  echo "+ $*" >&2
  if [ "${DRY_RUN:-}" != 1 ]; then "$@"; fi
}

for cmd in fakecicost:koinon-fakecicost promgen:koinon-rt1-promgen; do
  name="rt1-${cmd%%:*}"
  (cd "$root" && KO_DOCKER_REPO="$repo/$name" run ko build --bare --platform "$platform" --tags "$tag" "./cmd/${cmd##*:}")
done

run docker buildx build --platform "$platform" --push -t "$repo/rt1-motherduck:$tag" "$root/test/fixtures/rt1/motherduck"
run docker buildx build --platform "$platform" --push -f "$root/test/fixtures/rt1/prometheus/Dockerfile.mcp" \
  -t "$repo/rt1-prometheus-mcp:$tag" "$root/test/fixtures/rt1/prometheus"
run crane copy "$prometheus" "$repo/rt1-prometheus:v3.7.3"
run crane copy "$searxng" "$repo/rt1-searxng:$searxng_tag"
run crane copy "$searxng_mcp" "$repo/rt1-searxng-mcp:$searxng_mcp_tag"

if [ "${DRY_RUN:-}" != 1 ]; then
  echo "digests:"
  for ref in rt1-fakecicost:$tag rt1-promgen:$tag rt1-motherduck:$tag rt1-prometheus-mcp:$tag rt1-prometheus:v3.7.3 rt1-searxng:$searxng_tag rt1-searxng-mcp:$searxng_mcp_tag; do
    echo "  $repo/${ref%%:*}@$(crane digest "$repo/$ref")"
  done
fi
