# shellcheck shell=bash
# SPDX-License-Identifier: Apache-2.0
#
# Parallel parts for test/e2e/kind/up.sh (#572); sourced, not run. hack/e2e-parts-test.sh tests it offline.
# `bg` starts a part, `wait_all` fails when any part failed, `kill_parts` ends every part that still runs.
# Needs bash 5.1 (`wait -n -p`); the caller has the bash 5 guard (ADR 0090).
#
# Each part runs in its own process group (`set -m`: pgid = the part's pid; portable to macOS, where setsid is
# missing). A part can be a shell function (a bash subshell) whose child (helm --wait, up to 5 minutes) is not
# the tracked pid, so signalling the pid alone would leave the child running and changing the cluster during
# diagnostics and `kind delete`. Signalling the group ends the whole tree.

bg_pids=()
bg_names=()

bg() {
  set -m
  "$@" &
  bg_pids+=($!)
  bg_names+=("$*")
  set +m
}

# Ends the process group of every part still tracked and reaps them. Safe to call twice and from an EXIT trap.
kill_parts() {
  local p
  for p in ${bg_pids[@]+"${bg_pids[@]}"}; do kill -TERM -- "-$p" 2>/dev/null || true; done
  for p in ${bg_pids[@]+"${bg_pids[@]}"}; do wait "$p" 2>/dev/null || true; done
  bg_pids=()
  bg_names=()
}

# Waits for the parts as they finish. When one fails it ends the others (whole process trees) at once and exits
# with a message that names the failed part, instead of waiting for the slow ones first.
wait_all() {
  local done_pid rc i
  while [[ ${#bg_pids[@]} -gt 0 ]]; do
    rc=0
    wait -n -p done_pid "${bg_pids[@]}" || rc=$?
    for i in "${!bg_pids[@]}"; do
      [[ ${bg_pids[i]} == "${done_pid:-}" ]] || continue
      if [[ $rc -ne 0 ]]; then
        echo "up.sh: background part failed (exit $rc): ${bg_names[i]}" >&2
        kill_parts
        exit 1
      fi
      unset 'bg_pids[i]' 'bg_names[i]'
    done
    bg_pids=(${bg_pids[@]+"${bg_pids[@]}"})
    bg_names=("${bg_names[@]+"${bg_names[@]}"}")
  done
}
