#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds the RT1 fixture images locally, loads them into the kind cluster kind-$CLUSTER (default
# koinon-dev, `make dev-kind-up`) and applies test/fixtures/rt1/overlays/kind: the fixtures in
# namespace koinon-fixtures with fake Secrets. Needs docker, kind, ko and kubectl; no registry. The
# three upstream images (Prometheus, SearXNG, mcp-searxng) are pulled by the node, anonymously.
# See test/fixtures/rt1/README.md and docs/dev/real-model-tests.md. DRY_RUN=1 prints the commands only.
set -euo pipefail

CLUSTER=${CLUSTER:-koinon-dev}
CTX="kind-${CLUSTER}"
platform="linux/$(go env GOARCH)"
root="$(cd "$(dirname "$0")/.." && pwd)"

run() {
  echo "+ $*" >&2
  if [ "${DRY_RUN:-}" != 1 ]; then "$@"; fi
}

if [ "${DRY_RUN:-}" != 1 ]; then
  kind get clusters 2>/dev/null | grep -qx "$CLUSTER" || { echo "no kind cluster $CLUSTER: run 'make dev-kind-up' first" >&2; exit 1; }
fi

cd "$root"
for cmd in fakecicost:koinon-fakecicost promgen:koinon-rt1-promgen fakemodel:koinon-fakemodel; do
  name="rt1-${cmd%%:*}"
  KO_DOCKER_REPO="kind.local/$name" KIND_CLUSTER_NAME="$CLUSTER" run ko build --bare --platform "$platform" --tags dev "./cmd/${cmd##*:}"
done
for img in rt1-motherduck:test/fixtures/rt1/motherduck/Dockerfile:test/fixtures/rt1/motherduck \
           rt1-prometheus-mcp:test/fixtures/rt1/prometheus/Dockerfile.mcp:test/fixtures/rt1/prometheus; do
  IFS=: read -r name dockerfile context <<<"$img"
  run docker build --platform "$platform" -f "$dockerfile" -t "kind.local/$name:dev" "$context"
  run kind load docker-image --name "$CLUSTER" "kind.local/$name:dev"
done

# The Secrets in the overlay are fake placeholders (secrets.example.yaml).
run kubectl --context "$CTX" apply -k test/fixtures/rt1/overlays/kind
run kubectl --context "$CTX" -n koinon-fixtures rollout status deploy --timeout=300s
