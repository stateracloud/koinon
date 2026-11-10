#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# E1 quickstart e2e on kind (plan item E1, ADR 0049). From nothing to an approved pull request:
#
#   1. prerequisites (not on the quickstart clock): kind/up.sh;
#   2. images: Koinon, the fake model server and the fake GitHub with ko, the OpenCode flavor
#      with buildx, all loaded into the kind node; the fakes deployed in koinon-e2e-fakes;
#   3. the quickstart's shell commands (plan item DOC1, ADR 0108): the blocks of
#      test/e2e/commands/quickstart.e1, run by name through hack/docs-e1.sh. The docs site
#      (stateracloud/koinon-docs) shows the same commands and its CI compares its page with that file. They create the namespace, copy the OpenShell client
#      certificate, (SUITE=go) declare the model provider with a labelled Secret and `helm install`
#      charts/koinon (runc, CNPG, the fakes as model and GitHub). SUITE=web adds the provider through
#      the Providers page instead, as quickstart step 4 does. Only three things differ from a
#      reader's run: values.yaml is test/e2e/kind/koinon-values.yaml, model-provider.yaml is
#      test/e2e/kind/model-provider.yaml (the fake model), and kubectl/helm are wrappers that add
#      the kind context;
#      hack/e2e-commands-check.sh fails when this script and the file disagree;
#   4. SUITE=go (default): the Go driver in test/e2e drives the quickstart steps through the API
#      and prints the wall clock of each step and the total from `helm install`.
#      SUITE=web: Playwright (web/e2e-real, old issue 50) drives the same install through the
#      browser: login, GitHub connection, agent, task, streamed reply, approval, terminal run.
#      The two suites each need a fresh install (the bootstrap password is one-time), so run one
#      per cluster.
#
# Only the kube context kind-$CLUSTER is used; every kubectl/helm call names it. The cluster is
# deleted at the end unless KEEP=1.
#
#   SUITE_LABEL=smoke runs only the Ginkgo specs with that label (SUITE=go; smoke is steps 1-6).
#   FLAVOR_CACHE=<dir> imports the flavor build's layer cache from <dir>/opencode (needs the docker-container driver).
#   FLAVOR_PULL_REPO=<repo> pulls the flavor image <repo>:<hack/flavor-tag.sh tag> instead of building it.
#   BACKUP_RESTORE=1 (SUITE=go) adds test/e2e/backup-restore.sh after the driver: the command file
#   test/e2e/commands/backup-restore.e1 backs up the keys and the database, the install is
#   changed, then the file restores both and the script checks the install is the one that was backed up
#   (ADR 0085 decision 18, #90). The nightly run sets it; a pull request's run does not.
#   ESCAPE_MCP=1 (SUITE=go) also runs the escape suite's leak guard case (test/escape, label leakguard)
#   from a real sandbox against this install, inside step 6b while write_item and fetch_url are auto
#   (test/e2e/escape-mcp.sh calls test/escape/kind.sh; #348). e2e-kind.yml sets it.
#   ESCAPE_OPENSHELL=1 (default; SUITE=go) runs the escape suite's OpenShell cases against the install after the
#   Go driver (test/e2e/escape-openshell.sh; #109): the chart's sandbox admission policy is live, so the
#   real supervisor and workload pods are admitted by it and the escape cases are refused. 0 leaves it out
#   (about 4 to 8 minutes of wall clock).
#   CLUSTER=koinon-e1  SUITE=go|web  KEEP=0  SKIP_UP=0  SKIP_BUILD=0  OUT=<dir for logs and timings>
#
# The quickstart opens the API on localhost:8080 (the doc's port-forward block), so that port must be
# free. API_PORT=<n> changes the local end of the port-forward for the test only; the doc's block
# stays 8080.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

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
MCP_PORT=${MCP_PORT:-18091}
MODEL_PORT=${MODEL_PORT:-18092}
for port in "$API_PORT" "$GH_PORT" "$MCP_PORT" "$MODEL_PORT"; do
  if nc -z 127.0.0.1 "$port" 2>/dev/null; then echo "port $port is in use; run.sh port-forwards to it" >&2; exit 1; fi
done

root="$(cd "$(dirname "$0")/../.." && pwd)"
here="$root/test/e2e"
OUT=${OUT:-$(mktemp -d "${TMPDIR:-/tmp}/run.XXXXXX")}
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
# The doc's port-forward is a bare 8080 (or 8080:8080); with API_PORT set, only its local end changes.
if [[ $API_PORT != 8080 ]]; then
  cat >"$OUT/bin/kubectl" <<WRAP
#!/usr/bin/env bash
args=()
for a in "\$@"; do
  case \$a in 8080 | 8080:8080) a=$API_PORT:8080 ;; esac
  args+=("\$a")
done
exec $(command -v kubectl) --context $CTX "\${args[@]}"
WRAP
fi
# The quickstart's working directory: the repository's chart and the values file.
ln -sfn "$root/charts" "$OUT/quickstart/charts"
cp "$here/kind/koinon-values.yaml" "$OUT/quickstart/values.yaml"
cp "$here/kind/model-provider.yaml" "$OUT/quickstart/model-provider.yaml"
# e1 runs one quickstart block by name; `e1 run <name>` is a plain command, so it can be backgrounded
# and killed (the block's last command is exec'd).
e1_env=(env "PATH=$OUT/bin:$PATH" "E1_WORKDIR=$OUT/quickstart" "E1_FILE=$root/test/e2e/commands/quickstart.e1")
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
  for c in fakemodel fakegithub fakemcp; do
    "${K[@]}" -n "$FAKES" logs "deploy/$c" --tail=-1 >"$d/$c.log" 2>&1 || true
  done
  "${K[@]}" -n openshell logs statefulset/openshell --tail=-1 >"$d/openshell.log" 2>&1 || true
  # Sandbox namespaces (old issue 159): pod status with exit codes and termination messages. Koinon's
  # admission policy denies pods/log there, so the containers' output is read on the kind node
  # with crictl, below the API.
  local ns
  for ns in $("${K[@]}" get ns -o name 2>/dev/null | sed -n 's|^namespace/\(openshell-openshell-.*\)|\1|p'); do
    "${K[@]}" -n "$ns" get pods,sandboxes.agents.x-k8s.io -o yaml >"$d/sandbox-$ns.yaml" 2>&1 || true
  done
  local node="${CLUSTER}-control-plane" id
  docker exec "$node" crictl ps -a >"$d/crictl-ps.txt" 2>&1 || true
  for id in $(docker exec "$node" crictl ps -a -q 2>/dev/null); do
    case "$(docker exec "$node" crictl inspect -o go-template --template '{{index .status.labels "io.kubernetes.pod.namespace"}}' "$id" 2>/dev/null)" in
      openshell-openshell-*) docker exec "$node" crictl logs "$id" >"$d/container-$id.log" 2>&1 || true ;;
    esac
  done
  docker exec "$node" dmesg >"$d/node-dmesg.txt" 2>&1 || true
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
  # The chart's images are <image.registry>/api, /reconciler and /agentd (#167), so each is built --bare
  # into its own repository; the fakes keep ko's import-path names.
  for pair in api:./cmd/koinon-api reconciler:./cmd/koinon-reconciler agentd:./cmd/koinon-agentd; do
    KO_DOCKER_REPO="kind.local/${pair%%:*}" KIND_CLUSTER_NAME="$CLUSTER" "${KO:-ko}" build --bare --tags e2e \
      --platform "linux/$arch" "${pair#*:}"
  done
  KO_DOCKER_REPO=kind.local KIND_CLUSTER_NAME="$CLUSTER" "${KO:-ko}" build --base-import-paths --tags e2e \
    --platform "linux/$arch" ./cmd/koinon-fakemodel ./cmd/koinon-fakegithub ./cmd/koinon-fakemcp
  done_step "Images: ko build and load (6 binaries)"

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
  done_step "Image: OpenCode flavor (pull or buildx) and kind load"
fi

step "fakes ($FAKES)"
"${K[@]}" apply -f "$here/kind/fakes.yaml"
"${K[@]}" -n "$FAKES" rollout restart deploy/fakemodel deploy/fakegithub deploy/fakemcp >/dev/null
"${K[@]}" -n "$FAKES" rollout status deploy/fakemodel deploy/fakegithub deploy/fakemcp --timeout=120s
done_step "Fakes deployed"

# The quickstart blocks (test/e2e/commands/quickstart.e1), as the docs show them. The Go suite declares the model
# provider with a labelled Secret (the docs' GitOps alternative): model-provider.yaml declares the
# fake model server instead of OpenRouter. The browser suite adds the same provider in the UI.
step "quickstart: namespace, OpenShell client certificate, model provider"
e1 namespace
e1 client-cert
if [[ $SUITE == go ]]; then
  e1 model-provider
  # A second, direct Anthropic provider on the same fake server (ADR 0069): not part of the quickstart.
  "${K[@]}" apply -f "$here/kind/model-provider-anthropic.yaml"
fi
done_step "Namespace, client certificate and model provider (quickstart blocks)"

# Build what the drivers need before the clock starts: compiling the Go driver (a cold build cache
# after a dependency change takes minutes) or installing the browser is not part of the quickstart
# a user follows.
case "$SUITE" in
go)
  step "build the quickstart driver (go test -c)"
  go test -c -o "$OUT/bin/e2e.test" ./test/e2e/
  done_step "Quickstart driver build"
  ;;
web)
  step "install the browser suite (pnpm, playwright)"
  # The web toolchain image (hack/e2e-toolchain/web.Dockerfile) has Chromium and its system packages
  # under PLAYWRIGHT_BROWSERS_PATH: then only check the browser (a no-op at the same Playwright
  # version), without --with-deps, which would run apt.
  if [[ -n ${PLAYWRIGHT_BROWSERS_PATH:-} ]] && compgen -G "$PLAYWRIGHT_BROWSERS_PATH/chromium-*" >/dev/null; then
    (cd web && pnpm install --frozen-lockfile && pnpm exec playwright install chromium)
  else
    (cd web && pnpm install --frozen-lockfile && pnpm exec playwright install --with-deps chromium)
  fi
  done_step "Browser suite install"
  ;;
esac

# The quickstart clock starts here (ADR 0049).
step "quickstart: helm install"
T0=$(date +%s)
e1 install
done_step "helm install --wait"

"${e1_env[@]}" "$root/hack/docs-e1.sh" run open >"$OUT/pf-api.log" 2>&1 &
pids+=($!)
"${K[@]}" -n "$FAKES" port-forward svc/fakegithub "$GH_PORT:8080" >"$OUT/pf-fakegithub.log" 2>&1 &
pids+=($!)
"${K[@]}" -n "$FAKES" port-forward svc/fakemcp "$MCP_PORT:8080" >"$OUT/pf-fakemcp.log" 2>&1 &
pids+=($!)
"${K[@]}" -n "$FAKES" port-forward svc/fakemodel "$MODEL_PORT:8080" >"$OUT/pf-fakemodel.log" 2>&1 &
pids+=($!)
for port in "$API_PORT" "$GH_PORT" "$MCP_PORT" "$MODEL_PORT"; do
  for _ in $(seq 60); do nc -z 127.0.0.1 "$port" 2>/dev/null && break; sleep 0.5; done
  nc -z 127.0.0.1 "$port" || { echo "port-forward to $port did not come up" >&2; exit 1; }
done

password=$(e1 password)
case "$SUITE" in
go)
  step "quickstart driver (go test ./test/e2e)"
  KOINON_E2E_API_URL="http://127.0.0.1:$API_PORT" \
  KOINON_E2E_FAKEGITHUB_URL="http://127.0.0.1:$GH_PORT" \
  KOINON_E2E_FAKEMCP_URL="http://127.0.0.1:$MCP_PORT" \
  KOINON_E2E_FAKEMODEL_URL="http://127.0.0.1:$MODEL_PORT" \
  KOINON_E2E_KUBECTL="$OUT/bin/kubectl" \
  KOINON_E2E_PASSWORD="$password" \
  KOINON_E2E_ESCAPE_HOOK="$([[ ${ESCAPE_MCP:-0} == 1 ]] && echo "$here/escape-mcp.sh" || true)" \
  CLUSTER="$CLUSTER" \
  KOINON_E2E_T0="$T0" \
  KOINON_E2E_REPORT="$OUT/quickstart-timings.md" \
    bash -c 'cd "$1/test/e2e" && exec "$2" -test.timeout 30m -ginkgo.v ${3:+-ginkgo.label-filter="$3"}' _ "$root" "$OUT/bin/e2e.test" "${SUITE_LABEL:-}" 2>&1 | tee "$OUT/driver.log"
  done_step "Quickstart driver"
  ;;
web)
  step "browser e2e (playwright, web/e2e-real)"
  (cd web && KOINON_API_URL="http://127.0.0.1:$API_PORT" \
    KOINON_E2E_FAKEGITHUB_URL="http://127.0.0.1:$GH_PORT" \
    KOINON_E2E_PASSWORD="$password" \
    KOINON_E2E_FAKEMODEL_URL="http://fakemodel.$FAKES.svc:8080/v1" \
    PLAYWRIGHT_OUTPUT_DIR="$OUT/playwright" \
    pnpm exec playwright test -c playwright.real.config.ts) 2>&1 | tee "$OUT/web.log"
  done_step "Browser e2e"
  echo "Browser e2e (Playwright, web/e2e-real) passed in $(( $(date +%s) - ts )) s; details in web.log." >"$OUT/quickstart-timings.md"
  ;;
esac

if [[ $SUITE == go && ${ESCAPE_OPENSHELL:-1} == 1 ]]; then
  step "escape suite, OpenShell cases (test/escape, chart policy live)"
  CLUSTER="$CLUSTER" KOINON_NAMESPACE="$NS" "$here/escape-openshell.sh" 2>&1 | tee "$OUT/escape-openshell.log"
  done_step "Escape suite (OpenShell cases)"
fi

if [[ ${BACKUP_RESTORE:-0} == 1 ]]; then
  [[ $SUITE == go ]] || { echo "BACKUP_RESTORE=1 needs SUITE=go" >&2; exit 2; }
  step "backup and restore (commands/backup-restore.e1)"
  OUT="$OUT" "$here/backup-restore.sh" 2>&1 | tee "$OUT/backup-restore.log"
  done_step "Backup, keys export, restore and verify"
  cat "$OUT/backup-restore/timings.md" >>"$OUT/quickstart-timings.md"
fi

status=0
echo
echo "| Phase | Wall clock |"
echo "| --- | --- |"
printf '%s\n' "${summary[@]}"
echo "| **Whole script** | **$(( $(date +%s) - t_all )) s** |"
echo
echo "Quickstart steps (from helm install):"
cat "$OUT/quickstart-timings.md"
