#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# E1 quickstart e2e on kind (plan item E1, ADR 0049). From nothing to an approved pull request:
#
#   1. prerequisites (not on the quickstart clock): kind/up.sh;
#   2. images: Koinon, the fake model server and the fake GitHub with ko, the OpenCode flavor
#      with buildx, all loaded into the kind node; the fakes deployed in koinon-e2e-fakes;
#   3. the quickstart's shell commands (plan item DOC1): the blocks marked `sh e1=<name>` in
#      docs/site/src/content/docs/quickstart.md, run by name through hack/docs-e1.sh, so the docs
#      cannot drift from this test. They create the namespace, copy the OpenShell client
#      certificate, declare the model provider with a labelled Secret and `helm install`
#      charts/koinon (runc, CNPG, the fakes as model and GitHub). Only three things differ from a
#      reader's run: values.yaml is test/e2e/kind/koinon-values.yaml, model-provider.yaml is
#      test/e2e/kind/model-provider.yaml (the fake model), and kubectl/helm are wrappers that add
#      the kind context;
#      hack/docs-check.sh (make docs-check) fails when this script and the doc disagree;
#   4. SUITE=go (default): the Go driver in test/e2e drives the quickstart steps through the API
#      and prints the wall clock of each step and the total from `helm install`.
#      SUITE=web: Playwright (web/e2e-real, issue #50) drives the same install through the
#      browser: login, GitHub connection, agent, ticket, streamed reply, approval, terminal run.
#      The two suites each need a fresh install (the bootstrap password is one-time), so run one
#      per cluster.
#
# Only the kube context kind-$CLUSTER is used; every kubectl/helm call names it. The cluster is
# deleted at the end unless KEEP=1.
#
#   CLUSTER=koinon-e1  SUITE=go|web  KEEP=0  SKIP_UP=0  SKIP_BUILD=0  OUT=<dir for logs and timings>
#
# The quickstart opens the API on localhost:8080 (the doc's port-forward block), so that port must be
# free. API_PORT=<n> changes the local end of the port-forward for the test only; the doc's block
# stays 8080.
set -euo pipefail

CLUSTER=${CLUSTER:-koinon-e1}
CTX="kind-${CLUSTER}"
NS=koinon
FAKES=koinon-e2e-fakes
KEEP=${KEEP:-0}
SUITE=${SUITE:-go}
case "$SUITE" in go | web) ;; *) echo "SUITE must be go or web" >&2; exit 2 ;; esac
SKIP_UP=${SKIP_UP:-0}
SKIP_BUILD=${SKIP_BUILD:-0}
# The quickstart's port-forward block (docs) uses 8080; run.sh checks that this port is free.
API_PORT=${API_PORT:-8080}
GH_PORT=${GH_PORT:-18090}
for port in "$API_PORT" "$GH_PORT"; do
  if nc -z 127.0.0.1 "$port" 2>/dev/null; then echo "port $port is in use; run.sh port-forwards to it" >&2; exit 1; fi
done

root="$(cd "$(dirname "$0")/../.." && pwd)"
here="$root/test/e2e"
OUT=${OUT:-$(mktemp -d)}
mkdir -p "$OUT"
K=(kubectl --context "$CTX")
cd "$root"

# The quickstart blocks call plain `kubectl` and `helm`. These wrappers (first on PATH for
# docs-e1.sh only) pin them to the kind context, so a stray current context is never touched.
mkdir -p "$OUT/bin" "$OUT/quickstart"
for tool in kubectl helm; do
  flag=--context; [[ $tool == helm ]] && flag=--kube-context
  printf '#!/usr/bin/env bash\nexec %s %s %s "$@"\n' "$(command -v "$tool")" "$flag" "$CTX" >"$OUT/bin/$tool"
  chmod +x "$OUT/bin/$tool"
done
# The doc's port-forward is 8080:8080; with API_PORT set, only its local end changes.
if [[ $API_PORT != 8080 ]]; then
  cat >"$OUT/bin/kubectl" <<WRAP
#!/usr/bin/env bash
args=()
for a in "\$@"; do args+=("\${a/#8080:8080/$API_PORT:8080}"); done
exec $(command -v kubectl) --context $CTX "\${args[@]}"
WRAP
fi
# The quickstart's working directory: the repository's chart and the values file.
ln -sfn "$root/charts" "$OUT/quickstart/charts"
cp "$here/kind/koinon-values.yaml" "$OUT/quickstart/values.yaml"
cp "$here/kind/model-provider.yaml" "$OUT/quickstart/model-provider.yaml"
# e1 runs one quickstart block by name; `e1 run <name>` is a plain command, so it can be backgrounded
# and killed (the block's last command is exec'd).
e1_env=(env "PATH=$OUT/bin:$PATH" "E1_WORKDIR=$OUT/quickstart")
e1() { "${e1_env[@]}" "$root/hack/docs-e1.sh" run "$1"; }

pids=()
status=1
summary=()
step() { echo "== $(date -u +%H:%M:%SZ) $*"; ts=$(date +%s); }
done_step() {
  local d=$(( $(date +%s) - ts ))
  echo "-- $1: ${d}s"
  summary+=("| $1 | ${d} s |")
}

diagnostics() {
  local d="$OUT/diagnostics"
  mkdir -p "$d"
  "${K[@]}" get pods -A -o wide >"$d/pods.txt" 2>&1 || true
  "${K[@]}" get events -A --sort-by=.lastTimestamp >"$d/events.txt" 2>&1 || true
  for c in api reconciler agentd gateway; do
    "${K[@]}" -n "$NS" logs "deploy/koinon-$c" --all-containers --tail=-1 >"$d/koinon-$c.log" 2>&1 || true
  done
  for c in fakemodel fakegithub; do
    "${K[@]}" -n "$FAKES" logs "deploy/$c" --tail=-1 >"$d/$c.log" 2>&1 || true
  done
  "${K[@]}" -n openshell logs statefulset/openshell --tail=-1 >"$d/openshell.log" 2>&1 || true
  echo "diagnostics in $d"
}

cleanup() {
  for p in ${pids[@]+"${pids[@]}"}; do kill "$p" 2>/dev/null || true; done
  if [[ $status -ne 0 ]]; then diagnostics; fi
  if [[ "$KEEP" != 1 ]]; then "$here/kind/down.sh" || true; fi
  echo "logs and timings in $OUT"
  exit "$status"
}
trap cleanup EXIT

t_all=$(date +%s)

if [[ "$SKIP_UP" != 1 ]]; then
  step "prerequisites (kind/up.sh)"
  CLUSTER="$CLUSTER" "$here/kind/up.sh" 2>&1 | tee "$OUT/up.log"
  done_step "Prerequisites: kind, kube-network-policies, CNPG, Agent Sandbox, runc, OpenShell"
fi

if [[ "$SKIP_BUILD" != 1 ]]; then
  arch=$(go env GOARCH)
  step "Koinon images (ko, linux/$arch) into kind"
  KO_DOCKER_REPO=kind.local KIND_CLUSTER_NAME="$CLUSTER" "${KO:-ko}" build --base-import-paths --tags e2e \
    --platform "linux/$arch" ./cmd/koinon-api ./cmd/koinon-reconciler ./cmd/koinon-agentd \
    ./cmd/koinon-fakemodel ./cmd/koinon-fakegithub
  done_step "Images: ko build and load (5 binaries)"

  step "OpenCode flavor (buildx) into kind"
  docker buildx build --load --build-context common=flavors/common \
    --tag kind.local/flavor-opencode:e2e flavors/opencode
  kind load docker-image --name "$CLUSTER" kind.local/flavor-opencode:e2e
  done_step "Image: OpenCode flavor buildx and kind load"
fi

step "fakes ($FAKES)"
"${K[@]}" apply -f "$here/kind/fakes.yaml"
"${K[@]}" -n "$FAKES" rollout restart deploy/fakemodel deploy/fakegithub >/dev/null
"${K[@]}" -n "$FAKES" rollout status deploy/fakemodel deploy/fakegithub --timeout=120s
done_step "Fakes deployed"

# The quickstart blocks, as the docs show them (quickstart.md). The model provider is a labelled
# Secret; here model-provider.yaml declares the fake model server instead of OpenRouter.
step "quickstart: namespace, OpenShell client certificate, model provider"
e1 namespace
e1 client-cert
e1 model-provider
done_step "Namespace, client certificate and model provider (quickstart blocks)"

# The quickstart clock starts here (ADR 0049).
step "quickstart: helm install"
T0=$(date +%s)
e1 install
done_step "helm install --wait"

"${e1_env[@]}" "$root/hack/docs-e1.sh" run open >"$OUT/pf-api.log" 2>&1 &
pids+=($!)
"${K[@]}" -n "$FAKES" port-forward svc/fakegithub "$GH_PORT:8080" >"$OUT/pf-fakegithub.log" 2>&1 &
pids+=($!)
for port in "$API_PORT" "$GH_PORT"; do
  for _ in $(seq 60); do nc -z 127.0.0.1 "$port" 2>/dev/null && break; sleep 0.5; done
  nc -z 127.0.0.1 "$port" || { echo "port-forward to $port did not come up" >&2; exit 1; }
done

password=$(e1 password)
case "$SUITE" in
go)
  step "quickstart driver (go test ./test/e2e)"
  KOINON_E2E_API_URL="http://127.0.0.1:$API_PORT" \
  KOINON_E2E_FAKEGITHUB_URL="http://127.0.0.1:$GH_PORT" \
  KOINON_E2E_PASSWORD="$password" \
  KOINON_E2E_T0="$T0" \
  KOINON_E2E_REPORT="$OUT/quickstart-timings.md" \
    go test -count=1 -timeout 30m ./test/e2e/ -ginkgo.v 2>&1 | tee "$OUT/driver.log"
  done_step "Quickstart driver"
  ;;
web)
  step "browser e2e (playwright, web/e2e-real)"
  (cd web && pnpm install --frozen-lockfile && pnpm exec playwright install --with-deps chromium)
  (cd web && KOINON_API_URL="http://127.0.0.1:$API_PORT" \
    KOINON_E2E_FAKEGITHUB_URL="http://127.0.0.1:$GH_PORT" \
    KOINON_E2E_PASSWORD="$password" \
    PLAYWRIGHT_OUTPUT_DIR="$OUT/playwright" \
    pnpm exec playwright test -c playwright.real.config.ts) 2>&1 | tee "$OUT/web.log"
  done_step "Browser e2e"
  echo "Browser e2e (Playwright, web/e2e-real) passed in $(( $(date +%s) - ts )) s; details in web.log." >"$OUT/quickstart-timings.md"
  ;;
esac

status=0
echo
echo "| Phase | Wall clock |"
echo "| --- | --- |"
printf '%s\n' "${summary[@]}"
echo "| **Whole script** | **$(( $(date +%s) - t_all )) s** |"
echo
echo "Quickstart steps (from helm install):"
cat "$OUT/quickstart-timings.md"
