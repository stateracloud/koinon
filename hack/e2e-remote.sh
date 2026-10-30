#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Run the kind e2e (`make test-e2e-kind`, the go suite) on a remote Docker host, or on the local
# Docker with --local. Both run it inside the pinned toolchain image of hack/e2e-toolchain (the Go
# from go.mod and the kind, ko, helm and kubectl versions CI uses), against that host's Docker
# socket, so the result does not depend on what is installed on the machine you type this on.
#
#   hack/e2e-remote.sh [--out DIR] [--dry-run]     remote: procoduck's testbox, or $KOINON_E2E_REMOTE
#   hack/e2e-remote.sh --local [--out DIR]         the same container on the local Docker
#
# Remote: pushes the current HEAD (committed work only) to a bare repository in the remote user's
# home, then over SSH takes the e2e lock, checks it out, builds the toolchain image if missing and
# runs the e2e. Output streams back; the timings, logs and diagnostics are copied to --out; the exit
# status is the e2e's. Kind clusters are removed on exit, also when the SSH connection drops.
# Nothing secret goes to the remote: the e2e needs none (fake model, fake GitHub, local registry).
# hack/needs-e2e.sh says whether a change needs this at all.
#
# Environment:
#   KOINON_E2E_REMOTE       ssh target (default koinon@testbox.lab.example.invalid)
#   KOINON_E2E_REMOTE_WORK  bare repository on the remote, relative to its home (default work/koinon-e2e.git)
#   KOINON_E2E_OUT          where results land locally (default .e2e-out/<run id>); --out overrides
#
# The container uses --network host: kind publishes the API server on the Docker host's loopback and
# run.sh port-forwards there. That works on Linux Docker and OrbStack; Docker Desktop needs its
# host networking enabled.
set -euo pipefail

REMOTE=${KOINON_E2E_REMOTE:-koinon@testbox.lab.example.invalid}
WORK=${KOINON_E2E_REMOTE_WORK:-work/koinon-e2e.git}
CLUSTER=koinon-e1 # test/e2e/run.sh's default
mode=remote
out=${KOINON_E2E_OUT:-}
dry=0
# stage-only arguments
ref="" sha="" run_id="" runs="" name="" src=""

usage() { sed -n '4,/^set -e/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case $1 in
  --local) mode=local ;;
  --out) out=${2:?--out needs a directory}; shift ;;
  --dry-run) dry=1 ;;
  --stage) mode=stage ;; # internal: runs on the remote, the script is piped in over ssh
  --ref) ref=$2; shift ;;
  --sha) sha=$2; shift ;;
  --run-id) run_id=$2; shift ;;
  -h | --help) usage; exit 0 ;;
  *) echo "e2e-remote: unknown argument $1" >&2; exit 2 ;;
  esac
  shift
done

# run executes a command, or with --dry-run prints it (shell-quoted) instead.
run() {
  if [[ $dry == 1 ]]; then printf '+'; printf ' %q' "$@"; printf '\n'; else "$@"; fi
}

docker_sock() {
  case ${DOCKER_HOST:-} in
  unix://*) echo "${DOCKER_HOST#unix://}" ;;
  *) echo /var/run/docker.sock ;;
  esac
}

# ---- toolchain container (runs wherever Docker is: the remote in stage mode, here with --local) ----

# toolchain_image <src>: builds the image if this Dockerfile and Go version have none; prints its tag.
toolchain_image() {
  local src=$1 gov dsum tag
  gov=$(awk '/^go /{print $2; exit}' "$src/go.mod")
  dsum=$( (cat "$src/hack/e2e-toolchain/Dockerfile"; echo "$gov") | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-12)
  tag="koinon-e2e-toolchain:go${gov}-${dsum}"
  if ! docker image inspect "$tag" >/dev/null 2>&1; then
    echo "== building the toolchain image $tag" >&2
    docker build -q --build-arg "GO_VERSION=$gov" -t "$tag" "$src/hack/e2e-toolchain" >&2
  fi
  echo "$tag"
}

# remove_clusters removes the e2e's kind cluster's node containers (what `kind delete cluster` does,
# without needing kind on the host).
remove_clusters() {
  local id
  while IFS= read -r id; do docker rm -f -v "$id" >/dev/null 2>&1 || true; done \
    < <(docker ps -aq --filter "label=io.x-k8s.kind.cluster=$CLUSTER")
}

# run_e2e <src> <out> <container name>: make test-e2e-kind in the toolchain image; returns its status.
# The container runs as root (kind and the Docker socket need it); it hands the files back to the
# invoking user so the host can delete them.
run_e2e() {
  local src=$1 outdir=$2 name=$3 image rc=0
  image=$(toolchain_image "$src")
  echo "== running the e2e in $image"
  docker run --rm --name "$name" --network host \
    -v "$(docker_sock):/var/run/docker.sock" \
    -v "$src:/work" -v "$outdir:/out" \
    -v koinon-e2e-gomod:/go/pkg/mod -v koinon-e2e-gocache:/root/.cache/go-build \
    -e OUT=/out -e "CLUSTER=$CLUSTER" -e KEEP=0 \
    "$image" bash -c 'make test-e2e-kind; rc=$?; chown -R "$0:$1" /work /out 2>/dev/null; exit $rc' \
    "$(id -u)" "$(id -g)" </dev/null || rc=$?
  return "$rc"
}

# ---- stage: runs on the remote ----

stage() {
  local out rc=0
  # Globals: the EXIT trap runs after this function has returned.
  runs="$HOME/e2e-runs" name="koinon-e2e-$run_id" src="$runs/$run_id/src"
  out="$runs/$run_id/out"
  mkdir -p "$runs" "$out"

  cleanup() {
    docker rm -f "$name" >/dev/null 2>&1 || true
    remove_clusters
    rm -rf "$src"
    find "$runs" -mindepth 1 -maxdepth 1 -mtime +3 -exec rm -rf {} + 2>/dev/null || true
  }
  trap cleanup EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM

  # One e2e at a time on the host (ports 8080 and 18090-2, one kind cluster, ~8 GiB).
  exec 9>"$HOME/.koinon-e2e.lock"
  if ! flock -n 9; then
    echo "waiting for the e2e lock (another e2e is running on $(hostname))"
    flock 9
    echo "got the e2e lock"
  fi

  # The lock is ours, so a koinon-e1 cluster left by a crashed run is stale.
  remove_clusters
  git clone -q --branch "$ref" "$HOME/$WORK" "$src"
  if [[ $(git -C "$src" rev-parse HEAD) != "$sha" ]]; then
    echo "e2e-remote: checked out $(git -C "$src" rev-parse HEAD), expected $sha" >&2
    return 1
  fi
  run_e2e "$src" "$out" "$name" || rc=$?
  return "$rc"
}

# ---- client ----

repo_root() { git rev-parse --show-toplevel; }

# fetch_results <run id> <out>: copy the remote run's output home. Best effort.
fetch_results() {
  local id=$1 dest=$2
  mkdir -p "$dest"
  run ssh -o BatchMode=yes "$REMOTE" "tar -C e2e-runs/$id/out -cf - . 2>/dev/null; rm -rf e2e-runs/$id" |
    if [[ $dry == 1 ]]; then cat; else tar -xf - -C "$dest" 2>/dev/null || true; fi
}

report_results() { # <out> <status>
  echo
  echo "results in $1"
  if [[ -f $1/quickstart-timings.md ]]; then cat "$1/quickstart-timings.md"; fi
  [[ -d $1/diagnostics ]] && echo "diagnostics in $1/diagnostics"
  echo "e2e exit status $2"
}

client_remote() {
  local root branch id rc=0
  root=$(repo_root)
  cd "$root"
  sha=$(git rev-parse HEAD)
  branch=$(git symbolic-ref -q --short HEAD || echo detached)
  ref="e2e/$(tr -c 'A-Za-z0-9._-' '-' <<<"$branch" | sed 's/^-*//; s/-*$//')"
  id="$(date +%Y%m%d-%H%M%S)-${sha:0:8}-$$"
  : "${out:=$root/.e2e-out/$id}"
  if [[ -n $(git status --porcelain --untracked-files=no) ]]; then
    echo "warning: uncommitted changes are not tested; the e2e runs commit ${sha:0:12}" >&2
  fi

  echo "== pushing ${sha:0:12} to $REMOTE:$WORK ($ref)"
  # The bare repository is created on first use; init on an existing one is harmless.
  run ssh -o BatchMode=yes "$REMOTE" "git init -q --bare $(printf %q "$WORK")"
  run git push -q --force "$REMOTE:$WORK" "HEAD:refs/heads/$ref"

  echo "== running the e2e on $REMOTE (run $id)"
  # This script is piped to the remote, saved to a temporary file and run there in stage mode, so
  # no command inside it can read the rest of the script from stdin. The remote shell's HUP, when
  # the connection drops, runs stage's cleanup.
  local args remote_cmd
  args=$(printf ' %q' --stage --ref "$ref" --sha "$sha" --run-id "$id")
  # shellcheck disable=SC2016 # expands on the remote
  remote_cmd="f=\$(mktemp) && cat >\"\$f\" && KOINON_E2E_REMOTE_WORK=$(printf %q "$WORK") bash \"\$f\"$args; rc=\$?; rm -f \"\$f\"; exit \$rc"
  if [[ $dry == 1 ]]; then
    printf '+ ssh -o BatchMode=yes -o ServerAliveInterval=30 %q %q < %q\n' "$REMOTE" "$remote_cmd" "$0"
  else
    ssh -o BatchMode=yes -o ServerAliveInterval=30 "$REMOTE" "$remote_cmd" <"$0" || rc=$?
  fi

  fetch_results "$id" "$out"
  report_results "$out" "$rc"
  return "$rc"
}

client_local() {
  local root id rc=0
  root=$(repo_root)
  cd "$root"
  sha=$(git rev-parse HEAD)
  id="$(date +%Y%m%d-%H%M%S)-${sha:0:8}-$$"
  : "${out:=$root/.e2e-out/$id}"
  name="koinon-e2e-$id"
  if [[ -n $(git status --porcelain --untracked-files=no) ]]; then
    echo "warning: uncommitted changes are not tested; the e2e runs commit ${sha:0:12}" >&2
  fi
  if [[ -n $(docker ps -aq --filter "label=io.x-k8s.kind.cluster=$CLUSTER") ]]; then
    echo "e2e-remote: a kind cluster $CLUSTER already exists here (another e2e?); delete it with: kind delete cluster --name $CLUSTER" >&2
    return 1
  fi
  if [[ $dry == 1 ]]; then
    echo "+ git archive $sha into a temporary directory"
    echo "+ docker run ... $(printf '%q ' make test-e2e-kind)(see run_e2e)"
    return 0
  fi
  mkdir -p "$out"
  src=$(mktemp -d "${TMPDIR:-/tmp}/koinon-e2e.XXXXXX") # global: the EXIT trap uses it
  trap 'docker rm -f "$name" >/dev/null 2>&1 || true; remove_clusters; rm -rf "$src"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  git archive "$sha" | tar -x -C "$src"
  run_e2e "$src" "$out" "$name" || rc=$?
  report_results "$out" "$rc"
  return "$rc"
}

case $mode in
remote) client_remote ;;
local) client_local ;;
stage) stage ;;
esac
