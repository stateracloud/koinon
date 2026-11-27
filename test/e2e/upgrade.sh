#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The N-1 to N upgrade e2e (issue 519). test/e2e/run.sh calls it, in place of its own install and driver passes,
# when UPGRADE_FROM=<release tag> is set (for example UPGRADE_FROM=v0.1.0-rc.27). The steps:
#
#   1. install N-1: the published chart (oci://ghcr.io/stateracloud/koinon/charts/koinon, tag $UPGRADE_FROM) with the
#      published images, and the values file of that tag's tree (test/e2e/kind/koinon-values.yaml) with the image
#      settings of the registry instead of the local one;
#   2. seed it with N-1's own quickstart driver (the smoke label of that tag's test/e2e, built from a git worktree), so
#      the seed never depends on a client of the new build talking to an older server;
#   3. snapshot the database (test/e2e/upgradecheck format): install ID, migration, agents, tasks, runs, sandboxes,
#      home checkpoints, keys;
#   4. `helm upgrade koinon charts/koinon -f values.yaml` to this build (the e2e images), wait for every rollout;
#   5. snapshot again and judge the two with the Ginkgo spec of test/e2e/upgrade_test.go (label upgrade);
#   6. run the run-1 story suite of this build on the upgraded install (UPGRADE_STORIES=0 leaves it out): a new run
#      on an upgraded install, with the fake model.
#
# It is not a required CI job: main decides how it is wired. The pinned scope is the install of the same minor line
# (ADR 0085, ADR 0099: no schema or upgrade promise before v0.3, so a red result before v0.3 is information).
#
# Skip, not guess: `upgrade.sh check` exits 77 with the reason when this machine cannot pull the N-1 chart or its
# images, or cannot get the tag. run.sh turns 77 into a skip (exit 0), or a failure when KOINON_REQUIRE lists
# e2e-upgrade or all (testenv semantics).
#
# Needs from run.sh (see its header): OUT (bin/kubectl and bin/helm pinned to the kind context, quickstart/values.yaml,
# bin/e2e.test built from this tree, faketls/), CLUSTER, the ports, FAKES and the fakes deployed.
# Only the kube context kind-$CLUSTER is used.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
V=${UPGRADE_FROM:?UPGRADE_FROM is the release tag to upgrade from, for example v0.1.0-rc.27}
CHART_REF=${UPGRADE_CHART_REF:-oci://ghcr.io/stateracloud/koinon/charts/koinon}
IMAGE_REGISTRY=${UPGRADE_IMAGE_REGISTRY:-ghcr.io/stateracloud/koinon}
NS=koinon
REL=koinon

[[ $V =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo "UPGRADE_FROM must look like v0.1.0-rc.27, got: $V" >&2; exit 2; }

# check: can this machine run the test? Exit 77 (skip) with the reason on the first thing it cannot do.
check() {
  local work err
  work=$(mktemp -d "${TMPDIR:-/tmp}/upgrade-check.XXXXXX")
  trap 'rm -rf "$work"' RETURN
  for tool in helm docker git go kubectl curl; do
    command -v "$tool" >/dev/null || { echo "SKIP: $tool is not installed"; return 77; }
  done
  git -C "$root" rev-parse -q --verify "refs/tags/$V^{commit}" >/dev/null 2>&1 ||
    git -C "$root" fetch --quiet --no-tags origin "refs/tags/$V:refs/tags/$V" 2>"$work/git.err" ||
    { echo "SKIP: tag $V is not in this clone and cannot be fetched: $(head -c 300 "$work/git.err")"; return 77; }
  # kind/up.sh pulls the Agent Sandbox and network-policy images from registry.k8s.io (some lane VMs cannot reach it).
  curl -sS -m 15 -o /dev/null "https://registry.k8s.io/v2/" 2>"$work/curl.err" ||
    { echo "SKIP: registry.k8s.io is not reachable from this machine, so kind/up.sh cannot start the cluster: $(head -c 200 "$work/curl.err")"; return 77; }
  helm pull "$CHART_REF" --version "$V" --destination "$work" >"$work/helm.out" 2>&1 ||
    { echo "SKIP: cannot pull the chart $CHART_REF at $V: $(head -c 300 "$work/helm.out")"; return 77; }
  # The images the chart runs at $V come from IMAGE_REGISTRY. One probe: if the API image cannot be read here, the
  # install of N-1 would hang in ImagePullBackOff, which is not a result about the upgrade.
  if err=$(docker manifest inspect "$IMAGE_REGISTRY/api:${UPGRADE_IMAGE_TAG:-${V#v}}" 2>&1 >/dev/null); then :; else
    echo "SKIP: cannot read $IMAGE_REGISTRY/api:${UPGRADE_IMAGE_TAG:-${V#v}}: $(head -c 300 <<<"$err")"; return 77
  fi
  echo "ok: tag, chart and images of $V are reachable"
}

if [[ ${1:-} == check ]]; then check; exit $?; fi

OUT=${OUT:?OUT is the directory run.sh logs to}
CLUSTER=${CLUSTER:?}
FAKES=${FAKES:-koinon-e2e-fakes}
API_PORT=${API_PORT:-8080}
GH_PORT=${GH_PORT:-18090}
MCP_PORT=${MCP_PORT:-18091}
MODEL_PORT=${MODEL_PORT:-18092}
K=("$OUT/bin/kubectl")
H=("$OUT/bin/helm")
[[ -x ${K[0]} && -x ${H[0]} ]] || { echo "$OUT/bin/kubectl or helm is missing: run test/e2e/run.sh" >&2; exit 2; }
[[ -x $OUT/bin/e2e.test ]] || { echo "$OUT/bin/e2e.test is missing: run.sh builds it (SUITE=go)" >&2; exit 2; }

work="$OUT/upgrade"
mkdir -p "$work"
pids=()
step() { echo "== $(date -u +%H:%M:%SZ) upgrade: $*"; }
fail() { echo "upgrade FAILED: $*" >&2; exit 1; }
cleanup() {
  for p in ${pids[@]+"${pids[@]}"}; do kill "$p" 2>/dev/null || true; done
  git -C "$root" worktree remove --force "$work/prev" 2>/dev/null || true
}
trap cleanup EXIT

primary() { "${K[@]}" -n "$NS" get pod -l "cnpg.io/cluster=$REL-pg,cnpg.io/instanceRole=primary" -o name; }
sql() { "${K[@]}" -n "$NS" exec "$(primary)" -c postgres -- psql --dbname=koinon -v ON_ERROR_STOP=1 -Atq -c "$1"; }

# collect prints the snapshot (format: test/e2e/upgradecheck). Every id list is sorted, so two reads are comparable.
collect() {
  # A run still going adds rows between the two reads. Wait for the runs to be terminal first.
  local inflight
  for _ in $(seq 1 60); do
    inflight=$(sql "SELECT count(*) FROM runs WHERE state NOT IN ('succeeded', 'failed', 'canceled')")
    [[ $inflight == 0 ]] && break
    sleep 5
  done
  [[ $inflight == 0 ]] || fail "$inflight run(s) are not terminal after 5 minutes: the snapshot would move"
  echo "install_id=$(sql 'SELECT install_id FROM install_identity')"
  echo "migration=$(sql 'SELECT max(version_id) FROM goose_db_version WHERE is_applied')"
  local name table col
  for spec in agents:agents:id tasks:tasks:id runs:runs:id sandboxes:agent_sandboxes:sandbox_name \
    home_checkpoints:home_checkpoints:id home_repositories:home_repositories:agent_id; do
    IFS=: read -r name table col <<<"$spec"
    echo "count.$name=$(sql "SELECT count(*) FROM $table")"
    echo "ids.$name=$(sql "SELECT coalesce(string_agg($col::text, ',' ORDER BY $col::text), '') FROM $table")"
  done
  # The sandboxes with their state: after an upgrade each must come back to the state it had (a reconcile that fails
  # on an existing object, issue 508, leaves them behind with every pod running).
  echo "sandbox_states=$(sql "SELECT coalesce(string_agg(sandbox_name || ':' || state::text, ',' ORDER BY sandbox_name), '') FROM agent_sandboxes")"
  # The keys: ID, state, fingerprint (no key material).
  local keys
  keys=$("${K[@]}" -n "$NS" exec "deploy/$REL-api" -- koinon-api keys status | awk 'NF >= 5 && $2 ~ /^(active|retained|MISSING)$/ { print "kek=" $1, $2, $NF }')
  [[ -n $keys ]] || fail "keys status lists no KEK: the format changed, and the key check would prove nothing"
  echo "$keys"
}

declare -A fwd_pid
forward() { # forward <name> <svc> <local> <ns>: a port-forward that is restarted after a rollout
  local name=$1 svc=$2 port=$3 ns=$4
  "${K[@]}" -n "$ns" port-forward "svc/$svc" "$port:8080" >"$work/pf-$name.log" 2>&1 &
  pids+=($!)
  fwd_pid[$name]=$!
  for _ in $(seq 60); do nc -z 127.0.0.1 "$port" 2>/dev/null && return 0; sleep 0.5; done
  fail "port-forward $name to $port did not come up"
}

step "N-1 is $V: tree, chart and values"
check || { rc=$?; exit "$rc"; }
git -C "$root" worktree add --detach "$work/prev" "refs/tags/$V" >/dev/null
helm pull "$CHART_REF" --version "$V" --untar --untardir "$work/prev-chart" >/dev/null
# The tag's own values file, pointed at the published images: no local registry, no e2e tag (the chart's tag is its appVersion).
sed -e "s#registry: localhost:5001#registry: $IMAGE_REGISTRY#" -e '/^  tag: e2e$/d' -e 's#pullPolicy: Always#pullPolicy: IfNotPresent#' \
  "$work/prev/test/e2e/kind/koinon-values.yaml" >"$work/values-prev.yaml"
if [[ -n ${UPGRADE_IMAGE_TAG:-} ]]; then
  sed "s#^  registry: .*#&\\
  tag: \"$UPGRADE_IMAGE_TAG\"#" "$work/values-prev.yaml" >"$work/values-prev.tmp" && mv "$work/values-prev.tmp" "$work/values-prev.yaml"
fi
grep -q "registry: $IMAGE_REGISTRY" "$work/values-prev.yaml" || fail "the values file of $V has no image registry line to point at $IMAGE_REGISTRY"
# A tag from #1295 on holds the egress CA placeholder too: fill it with this run's CA, as run.sh does (#1487).
(cd "$root" && go run ./test/e2e/kindvalues/fill-egress-ca -if-present -values "$work/values-prev.yaml" -ca "$OUT/faketls/ca.crt") ||
  fail "the egress CA of the values file of $V"
(cd "$work/prev" && go test -c -o "$OUT/bin/e2e-prev.test" ./test/e2e/) || fail "the quickstart driver of $V does not build"

step "install N-1 ($V)"
"${H[@]}" install "$REL" "$work/prev-chart/koinon" --namespace "$NS" -f "$work/values-prev.yaml" --wait --timeout 10m
password=$("${K[@]}" -n "$NS" get secret "$REL-bootstrap-admin" -o jsonpath='{.data.password}' | base64 -d)
new_password="${KOINON_E2E_NEW_PASSWORD:-e2e-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')}"
forward api "$REL-api" "$API_PORT" "$NS"
forward fakegithub fakegithub "$GH_PORT" "$FAKES"
forward fakemcp fakemcp "$MCP_PORT" "$FAKES"
forward fakemodel fakemodel "$MODEL_PORT" "$FAKES"

step "seed N-1 with its own quickstart driver (label smoke)"
# The driver of the tag runs in that tag's tree (its fixtures are relative paths).
prev_dir="$work/prev/test/e2e"
seed() {
  # The single quotes are intended: the inner bash expands $1..$3 (same pattern as run.sh).
  # shellcheck disable=SC2016
  env KOINON_E2E_API_URL="http://127.0.0.1:$API_PORT" \
    KOINON_E2E_FAKEGITHUB_URL="http://127.0.0.1:$GH_PORT" \
    KOINON_E2E_FAKEMCP_URL="https://127.0.0.1:$MCP_PORT" \
    KOINON_E2E_FAKEMCP_CA="$OUT/faketls/ca.crt" \
    KOINON_E2E_FAKEMODEL_URL="http://127.0.0.1:$MODEL_PORT" \
    KOINON_E2E_KUBECTL="$OUT/bin/kubectl" \
    KOINON_E2E_PASSWORD="$password" \
    KOINON_E2E_NEW_PASSWORD="$new_password" \
    KOINON_E2E_ESCAPE_HOOK="" \
    CLUSTER="$CLUSTER" \
    KOINON_E2E_T0="$(date +%s)" \
    KOINON_E2E_REPORT="$work/seed-timings.md" \
    bash -c 'cd "$1" && exec "$2" -test.timeout 30m -ginkgo.v -ginkgo.label-filter="$3"' _ "$1" "$2" "$3"
}
seed "$prev_dir" "$OUT/bin/e2e-prev.test" smoke 2>&1 | tee "$work/seed.log"

step "snapshot before the upgrade"
collect | tee "$work/state.before"

step "helm upgrade to this build"
"${H[@]}" upgrade "$REL" "$root/charts/koinon" --namespace "$NS" -f "$OUT/quickstart/values.yaml" --wait --timeout 10m
for d in $("${K[@]}" -n "$NS" get deploy -l "app.kubernetes.io/instance=$REL" -o name); do
  "${K[@]}" -n "$NS" rollout status "$d" --timeout=5m
done
# The API pod is new: the old port-forward points at a pod that is gone.
kill "${fwd_pid[api]}" 2>/dev/null || true
wait "${fwd_pid[api]}" 2>/dev/null || true
forward api "$REL-api" "$API_PORT" "$NS"

step "snapshot after the upgrade (the sandboxes must reconcile back to their state first)"
want=$(grep '^sandbox_states=' "$work/state.before")
for _ in $(seq 1 60); do
  [[ $(collect | grep '^sandbox_states=') == "$want" ]] && break
  sleep 5
done
collect | tee "$work/state.after"
[[ $(grep '^sandbox_states=' "$work/state.after") == "$want" ]] ||
  fail "the sandboxes did not return to their state within 5 minutes: before ${want#sandbox_states=}, after $(sed -n 's/^sandbox_states=//p' "$work/state.after")"

step "judge: the upgrade spec (label upgrade)"
(cd "$root/test/e2e" && KOINON_E2E_API_URL="http://127.0.0.1:$API_PORT" KOINON_E2E_KUBECTL="$OUT/bin/kubectl" \
  KOINON_E2E_UPGRADE_FROM="$V" KOINON_E2E_UPGRADE_BEFORE="$work/state.before" KOINON_E2E_UPGRADE_AFTER="$work/state.after" \
  KOINON_REQUIRE="${KOINON_REQUIRE:-}" \
  "$OUT/bin/e2e.test" -test.timeout 10m -ginkgo.v -ginkgo.label-filter=upgrade \
  -ginkgo.junit-report="$OUT/upgrade-junit.xml") 2>&1 | tee "$work/judge.log"

if [[ ${UPGRADE_STORIES:-1} == 1 ]]; then
  step "a new run on the upgraded install: the run-1 story suite of this build (label stories)"
  (cd "$root/test/e2e" && env KOINON_E2E_API_URL="http://127.0.0.1:$API_PORT" \
    KOINON_E2E_FAKEGITHUB_URL="http://127.0.0.1:$GH_PORT" \
    KOINON_E2E_FAKEMCP_URL="https://127.0.0.1:$MCP_PORT" KOINON_E2E_FAKEMCP_CA="$OUT/faketls/ca.crt" \
    KOINON_E2E_FAKEMODEL_URL="http://127.0.0.1:$MODEL_PORT" KOINON_E2E_KUBECTL="$OUT/bin/kubectl" \
    KOINON_E2E_PASSWORD="$password" KOINON_E2E_NEW_PASSWORD="$new_password" KOINON_E2E_ESCAPE_HOOK="" CLUSTER="$CLUSTER" \
    "$OUT/bin/e2e.test" -test.timeout 15m -ginkgo.v -ginkgo.label-filter=stories \
    -ginkgo.junit-report="$OUT/upgrade-stories-junit.xml") 2>&1 | tee "$work/stories.log"
fi

echo "Upgrade from $V to this build: install, seed, helm upgrade and convergence passed (state: $work/state.before, $work/state.after)." | tee "$OUT/upgrade/summary.md"
