#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds the RT1 fixture images locally, pushes them to the kind cluster's path on the local registry
# (<registry>/$CLUSTER, test/e2e/kind/clusterrepo.sh; #45, #601; nothing is `kind load`ed) and applies
# test/fixtures/rt1/overlays/kind: the fixtures in namespace koinon-fixtures with fake Secrets. The cluster is
# kind-$CLUSTER (default koinon-dev, `make dev-kind-up`, which also starts the registry). Needs docker, ko
# and kubectl. The three upstream images (Prometheus, SearXNG, mcp-searxng) are pulled by the node, anonymously.
# See test/fixtures/rt1/README.md and docs/dev/real-model-tests.md. DRY_RUN=1 prints the commands only.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

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
  REPO=$("$root/test/e2e/kind/clusterrepo.sh" "$CLUSTER")
else
  REPO="localhost:5001/$CLUSTER"
fi

cd "$root"
for cmd in fakecicost:koinon-fakecicost promgen:koinon-rt1-promgen fakemodel:koinon-fakemodel; do
  name="rt1-${cmd%%:*}"
  KO_DOCKER_REPO="$REPO/$name" run ko build --bare --platform "$platform" --tags dev "./cmd/${cmd##*:}"
done
for img in rt1-motherduck:test/fixtures/rt1/motherduck/Dockerfile:test/fixtures/rt1/motherduck \
           rt1-prometheus-mcp:test/fixtures/rt1/prometheus/Dockerfile.mcp:test/fixtures/rt1/prometheus; do
  IFS=: read -r name dockerfile context <<<"$img"
  run docker build --platform "$platform" -f "$dockerfile" -t "$REPO/$name:dev" "$context"
  run docker push -q "$REPO/$name:dev"
done

# The awk pass adds imagePullPolicy: Always after a registry image line, unless the same container already
# sets a policy (it looks at the keys before and after the image line, up to the container's edge).
rewrite() {
  awk -v repo="$REPO" '
    function ind(l) { match(l, /^ */); return RLENGTH }
    { sub(/localhost:5001\//, repo "/"); L[NR] = $0 }
    END {
      for (i = 1; i <= NR; i++) {
        print L[i]
        if (!match(L[i], /image: /) || index(substr(L[i], RSTART + 7), repo "/") != 1) continue
        n = RSTART - 1; has = 0
        # "image:" may follow a list dash ("- image: ..."): the keys of the container line up with it.
        for (j = i + 1; j <= NR; j++) { if (ind(L[j]) < n || (ind(L[j]) == n - 2 && L[j] ~ /^ *- /)) break; if (ind(L[j]) == n && L[j] ~ /^ *imagePullPolicy:/) has = 1 }
        for (j = i - 1; j >= 1 && !has; j--) { if (ind(L[j]) < n - 2) break; if (ind(L[j]) == n - 2 && L[j] ~ /^ *- /) { if (L[j] ~ /imagePullPolicy:/) has = 1; break } ; if (ind(L[j]) == n && L[j] ~ /^ *imagePullPolicy:/) has = 1 }
        if (!has) printf "%*simagePullPolicy: Always\n", n, ""
      }
    }'
}

# The overlay names the images under localhost:5001; this cluster's are under $REPO. The tag `dev` is reused
# and a pushed image is not reloaded into the node as `kind load` did, so the Pods pull it on every start.
# The Secrets in the overlay are fake placeholders (secrets.example.yaml).
echo "+ kubectl --context $CTX kustomize test/fixtures/rt1/overlays/kind | rewrite localhost:5001 to $REPO | kubectl apply -f -" >&2
if [ "${DRY_RUN:-}" != 1 ]; then
  kubectl --context "$CTX" kustomize test/fixtures/rt1/overlays/kind | rewrite | kubectl --context "$CTX" apply -f -
fi
run kubectl --context "$CTX" -n koinon-fixtures rollout status deploy --timeout=300s
