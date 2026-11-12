#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): the lab target is maintainer infrastructure; --local works from a fork.
#
# Run the kind e2e (`make test-e2e-kind`, the go suite) of the committed HEAD somewhere other than
# your shell's toolchain, two ways:
#
#   hack/e2e-remote.sh [--local] [--out DIR] [--dry-run]              the local Docker (default)
#   hack/e2e-remote.sh --ssh [--out DIR] [--dry-run]                  a Docker host over SSH ($KOINON_E2E_REMOTE)
#
# Other queues are not driven from this repository. This is for the dev loop and real-model runs, not merge
# evidence (ADR 0105): the merge queue's hosted `quickstart` is the one e2e proof. It may still post `e2e-lab`
# (hack/e2e-status.sh) as information.
#
# --ssh and --local run the pinned toolchain image of hack/e2e-toolchain (the Go from go.mod and the
# kind, ko, helm and kubectl versions CI uses) against that host's Docker socket, so the result does
# not depend on what is installed on the machine you type this on. Setting KOINON_E2E_REMOTE alone
# also selects --ssh.
#
# SSH: pushes the current HEAD (committed work only) to a unique ref (refs/e2e/<run id>) of a bare
# repository in the remote user's home, then over SSH takes the host-wide e2e lock, checks that
# commit out, builds the toolchain image if missing and runs the e2e. Output streams back; the
# timings, logs and diagnostics are copied to --out; the exit status is the e2e's. hack/needs-e2e.sh
# says whether a change needs this at all.
#
# ACCEPTED RISK: the e2e builds and runs the code under test as root, in a container that has the
# host's Docker socket and the host network (kind needs both). Run it only on a host you would let
# that code run on (a throwaway VM, your own machine), never on one with secrets. The Go module and
# build caches (Docker volumes koinon-e2e-gomod, koinon-e2e-gocache) are shared between runs, so
# one run's code can poison the next one's cache. Nothing secret is sent to the remote: the e2e
# needs none (fake model, fake GitHub).
#
# Cleanup guarantees: kind's cluster (koinon-e2e-runner, a name no human uses) is removed, the run
# container killed, files handed back to the invoking user and the lock released when the e2e ends,
# on Ctrl-C or TERM, and within about 10 s of the SSH connection dropping (a watchdog polls the
# sshd session). A kill -9 of the stage or a power loss is not covered: the next run removes a stale
# cluster once it holds the lock.
#
# Locking: one e2e at a time per host. Remote: flock on /tmp/koinon-e2e.lock (shared by every user
# of the host; override with KOINON_E2E_LOCK). --local uses the same file with flock, or a
# directory lock (/tmp/koinon-e2e.lock.d) where flock does not exist (macOS).
#
# Environment:
#   KOINON_E2E_REMOTE       ssh target for --ssh (no default)
#   KOINON_E2E_REMOTE_WORK  bare repository on the remote, relative to its home (default work/koinon-e2e.git)
#   KOINON_E2E_OUT          where results land locally (default .e2e-out/<run id>); --out overrides
#   KOINON_E2E_LOCK         lock file path on the host that runs the e2e
#   SUITE_LABEL=smoke       run only the smoke specs (steps 1-6 of the quickstart), about half the time
#
# The container uses --network host: kind publishes the API server on the Docker host's loopback and
# run.sh port-forwards there. That works on Linux Docker and OrbStack; Docker Desktop needs its
# host networking enabled. The Docker socket mounted into it is /var/run/docker.sock as the daemon
# sees it (the same on Linux, OrbStack and Docker Desktop), whatever DOCKER_HOST says here.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

SELF=$(cd "$(dirname "$0")" && pwd)/$(basename "$0") # absolute: we cd later
REMOTE=${KOINON_E2E_REMOTE:-}
WORK=${KOINON_E2E_REMOTE_WORK:-work/koinon-e2e.git}
LOCK=${KOINON_E2E_LOCK:-/tmp/koinon-e2e.lock}
CLUSTER=koinon-e2e-runner # dedicated: cleanup never touches a human's koinon-e1
mode=""
[[ -z $REMOTE ]] || mode=remote # KOINON_E2E_REMOTE alone selects --ssh
out=${KOINON_E2E_OUT:-}
dry=0
# stage-only arguments, and globals the EXIT traps use
ref="" sha="" run_id="" runs="" name="" src="" image="" run_pid="" lock_dir="" watch_pid="" OUT_DIR=""

usage() { sed -n '4,/^set -e/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case $1 in
  --local) mode=local ;;
  --ssh) mode=remote ;;
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

# ---- lock ----

# acquire_lock waits for the host-wide e2e lock. flock where it exists (fd 9, released when the
# process exits, whatever way); otherwise a directory with the owner's pid (stale ones are taken over).
acquire_lock() {
  if command -v flock >/dev/null 2>&1 && [[ -z ${KOINON_E2E_FORCE_MKDIR_LOCK:-} ]]; then
    exec 9>>"$LOCK"
    chmod 666 "$LOCK" 2>/dev/null || true # other users of the host lock it too
    if ! flock -n 9; then
      echo "waiting for the e2e lock (another e2e is running on $(hostname))"
      flock 9
      echo "got the e2e lock"
    fi
    return
  fi
  lock_dir="$LOCK.d"
  local waited=0
  until mkdir "$lock_dir" 2>/dev/null; do
    local owner
    owner=$(cat "$lock_dir/pid" 2>/dev/null || true)
    if [[ -n $owner ]] && ! kill -0 "$owner" 2>/dev/null; then rm -rf "$lock_dir"; continue; fi
    if [[ $waited == 0 ]]; then echo "waiting for the e2e lock (pid ${owner:-?} holds it)"; fi
    waited=1
    sleep 2
  done
  echo $$ >"$lock_dir/pid"
  [[ $waited == 0 ]] || echo "got the e2e lock"
}

release_lock() {
  if [[ -n $lock_dir ]]; then rm -rf "$lock_dir"; lock_dir=""; fi
}

# ---- toolchain container (runs wherever Docker is: the remote in stage mode, here with --local) ----

# toolchain_image <src>: builds the image if this Dockerfile has none; sets $image.
toolchain_image() {
  local s=$1 gov want dsum
  gov=$(awk '/^go /{print $2; exit}' "$s/go.mod")
  want=$(sed -n 's/^ARG GO_VERSION=//p' "$s/hack/e2e-toolchain/Dockerfile")
  if [[ $gov != "$want" ]]; then
    echo "e2e-remote: go.mod wants Go $gov but hack/e2e-toolchain/Dockerfile pins $want (update GO_VERSION and GO_DIGEST)" >&2
    return 1
  fi
  dsum=$({ cat "$s/hack/e2e-toolchain/Dockerfile"; } | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-12)
  image="koinon-e2e-toolchain:go${gov}-${dsum}"
  if ! docker image inspect "$image" >/dev/null 2>&1; then
    echo "== building the toolchain image $image" >&2
    docker build -q -t "$image" "$s/hack/e2e-toolchain" >&2
  fi
}

# remove_clusters removes the e2e's kind cluster's node containers (what `kind delete cluster` does,
# without needing kind on the host).
remove_clusters() {
  local id
  while IFS= read -r id; do docker rm -f -v "$id" >/dev/null 2>&1 || true; done \
    < <(docker ps -aq --filter "label=io.x-k8s.kind.cluster=$CLUSTER")
}

# fix_owner hands the run's files (written by root in the container) back to the invoking user.
# Runs from the EXIT trap, so also after the container was killed.
fix_owner() {
  [[ -n $image && -n $src ]] || return 0
  docker run --rm --entrypoint chown -v "$src:/work" -v "$OUT_DIR:/out" "$image" \
    -R "$(id -u):$(id -g)" /work /out >/dev/null 2>&1 || true
}

# run_e2e <src> <out> <container name>: make test-e2e-kind in the toolchain image; returns its status.
# The container runs in the background and we wait on it, so a TERM is handled at once.
run_e2e() {
  local s=$1 outdir=$2 cname=$3 rc=0
  OUT_DIR=$outdir
  toolchain_image "$s"
  echo "== running the e2e in $image"
  docker run --rm --name "$cname" --network host \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$s:/work" -v "$outdir:/out" \
    -v koinon-e2e-gomod:/go/pkg/mod -v koinon-e2e-gocache:/root/.cache/go-build \
    -e OUT=/out -e "CLUSTER=$CLUSTER" -e KEEP=0 ${SUITE_LABEL:+-e "SUITE_LABEL=$SUITE_LABEL"} \
    "$image" make test-e2e-kind </dev/null 9>&- &
  run_pid=$!
  wait "$run_pid" || rc=$?
  run_pid=""
  return "$rc"
}

# kill_run stops the e2e container and its client process.
kill_run() {
  [[ -z $run_pid ]] || kill "$run_pid" 2>/dev/null || true
  [[ -z $name ]] || docker rm -f "$name" >/dev/null 2>&1 || true
}

# ---- stage: runs on the remote ----

# sshd_ancestor prints the pid of the sshd process this shell descends from, if any.
sshd_ancestor() {
  local p=$$ comm
  while [[ $p -gt 1 ]]; do
    comm=$(ps -o comm= -p "$p" 2>/dev/null | tr -d ' ') || return 0
    case $comm in *sshd*) echo "$p"; return 0 ;; esac
    p=$(ps -o ppid= -p "$p" 2>/dev/null | tr -d ' ') || return 0
    [[ -n $p ]] || return 0
  done
}

# watchdog <pid to watch> <pid to TERM>: when the SSH session's sshd process is gone, stop the stage.
watchdog() {
  while kill -0 "$1" 2>/dev/null; do sleep 5; done
  echo "e2e-remote: the SSH connection is gone; stopping the e2e" >&2
  kill -TERM "$2" 2>/dev/null || true
}

stage_cleanup() {
  [[ -z $watch_pid ]] || kill "$watch_pid" 2>/dev/null || true
  kill_run
  remove_clusters
  fix_owner
  rm -rf "$src"
  git --git-dir="$HOME/$WORK" update-ref -d "refs/e2e/$run_id" 2>/dev/null || true
  find "$runs" -mindepth 1 -maxdepth 1 -mtime +3 -exec rm -rf {} + 2>/dev/null || true
  release_lock
}

stage() {
  local rc=0 target
  runs="$HOME/e2e-runs" name="koinon-e2e-$run_id" src="$runs/$run_id/src"
  mkdir -p "$runs/$run_id/out"
  trap stage_cleanup EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM

  target=${KOINON_E2E_WATCH_PID:-$(sshd_ancestor)}
  if [[ -n $target ]]; then
    watchdog "$target" $$ &
    watch_pid=$!
  fi

  acquire_lock
  # The lock is ours, so a cluster left by a crashed run is stale.
  remove_clusters
  mkdir -p "$src"
  git -C "$src" init -q
  git -C "$src" fetch -q "$HOME/$WORK" "+refs/e2e/$run_id:refs/e2e/run"
  git -C "$src" checkout -q --detach "$sha"
  if [[ $(git -C "$src" rev-parse HEAD) != "$sha" ]]; then
    echo "e2e-remote: checked out $(git -C "$src" rev-parse HEAD), expected $sha" >&2
    return 1
  fi
  run_e2e "$src" "$runs/$run_id/out" "$name" || rc=$?
  return "$rc"
}

# ---- client ----

# fetch_results <run id> <out>: copy the remote run's output home and drop its ref. Best effort.
fetch_results() {
  local id=$1 dest=$2
  mkdir -p "$dest"
  run ssh -o BatchMode=yes "$REMOTE" \
    "git --git-dir=$(printf %q "$WORK") update-ref -d refs/e2e/$id 2>/dev/null; tar -C e2e-runs/$id/out -cf - . 2>/dev/null; rm -rf e2e-runs/$id" |
    if [[ $dry == 1 ]]; then cat; else tar -xf - -C "$dest" 2>/dev/null || true; fi
}

report_results() { # <out> <status>
  echo
  echo "results in $1"
  if [[ -f $1/quickstart-timings.md ]]; then cat "$1/quickstart-timings.md"; fi
  [[ -d $1/diagnostics ]] && echo "diagnostics in $1/diagnostics"
  echo "e2e exit status $2"
}

warn_dirty() {
  if [[ -n $(git status --porcelain --untracked-files=no) ]]; then
    echo "warning: uncommitted changes are not tested; the e2e runs commit ${sha:0:12}" >&2
  fi
}

client_remote() {
  local root id rc=0
  root=$(git rev-parse --show-toplevel)
  cd "$root"
  sha=$(git rev-parse HEAD)
  id="$(date +%Y%m%d-%H%M%S)-${sha:0:8}-$$"
  ref="refs/e2e/$id" # unique per run: two runs of one branch never share a ref
  : "${out:=$root/.e2e-out/$id}"
  warn_dirty

  echo "== pushing ${sha:0:12} to $REMOTE:$WORK ($ref)"
  # The bare repository is created on first use; init on an existing one is harmless.
  run ssh -o BatchMode=yes "$REMOTE" "git init -q --bare $(printf %q "$WORK")"
  run git push -q "$REMOTE:$WORK" "HEAD:$ref"

  echo "== running the e2e on $REMOTE (run $id)"
  # This script is piped to the remote, saved to a temporary file and run there in stage mode, so
  # no command inside it can read the rest of the script from stdin.
  local args remote_cmd
  args=$(printf ' %q' --stage --sha "$sha" --run-id "$id")
  # shellcheck disable=SC2016 # expands on the remote
  remote_cmd="f=\$(mktemp \"\${TMPDIR:-/tmp}/koinon-e2e-stage.XXXXXX\") && cat >\"\$f\" && KOINON_E2E_REMOTE_WORK=$(printf %q "$WORK") bash \"\$f\"$args; rc=\$?; rm -f \"\$f\"; exit \$rc"
  if [[ $dry == 1 ]]; then
    printf '+ ssh -o BatchMode=yes -o ServerAliveInterval=30 %q %q < %q\n' "$REMOTE" "$remote_cmd" "$SELF"
  else
    ssh -o BatchMode=yes -o ServerAliveInterval=30 "$REMOTE" "$remote_cmd" <"$SELF" || rc=$?
  fi

  fetch_results "$id" "$out"
  report_results "$out" "$rc"
  return "$rc"
}

local_cleanup() {
  kill_run
  remove_clusters
  fix_owner
  rm -rf "$src"
  release_lock
}

client_local() {
  local root id rc=0
  root=$(git rev-parse --show-toplevel)
  cd "$root"
  sha=$(git rev-parse HEAD)
  id="$(date +%Y%m%d-%H%M%S)-${sha:0:8}-$$"
  : "${out:=$root/.e2e-out/$id}"
  name="koinon-e2e-$id"
  warn_dirty
  if [[ $dry == 1 ]]; then
    echo "+ lock $LOCK, git archive $sha into a temporary directory, then run_e2e (docker run ... make test-e2e-kind)"
    return 0
  fi
  mkdir -p "$out"
  src=$(mktemp -d "${TMPDIR:-/tmp}/koinon-e2e.XXXXXX")
  trap local_cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  acquire_lock
  remove_clusters # ours alone (koinon-e2e-runner), and we hold the lock: stale
  git archive "$sha" | tar -x -C "$src"
  run_e2e "$src" "$out" "$name" || rc=$?
  report_results "$out" "$rc"
  return "$rc"
}

case ${mode:-local} in
remote)
  [[ -n $REMOTE ]] || { echo "e2e-remote: --ssh needs KOINON_E2E_REMOTE (user@host)" >&2; exit 2; }
  client_remote ;;
local) client_local ;;
stage) stage ;;
esac
