#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The --argo mode of hack/e2e-remote.sh: run a make target (the kind e2e by default) of the committed
# HEAD on an Argo Workflows queue that you operate (the maintainers use their home lab, see
# docs/dev/maintainer-homelab.md). Nothing about a particular queue is tracked here: the endpoints
# come from the environment or from an untracked config file (hack/lab/config.sh; template
# hack/lab/lab.env.example), and the script exits 2 when they are missing. Steps: a git bundle of
# HEAD goes to an S3 bucket, the WorkflowTemplate is submitted through the Argo Server REST API (no
# argo CLI needed) on the slot with the fewest unfinished runs, and the run is followed to the end:
# stage lines while it runs, then the results archive (timings, logs, diagnostics) in --out and the
# run's exit status. Ctrl-C stops the run.
#
# Lanes: kind (default) submits the queue's WorkflowTemplate (koinon-make; target test-e2e-kind);
# light submits the manifest in KOINON_E2E_LIGHT_MANIFEST as an inline Workflow for targets without a
# kind cluster (default test-go; hack/lab/light-run.sh lists them) on its own light slots and caches.
# The manifest has the parameters target, ref, bundle, agent and slot (with an enum of the slot
# names), fetches the bundle, checks out `ref` and runs `hack/lab/light-run.sh $target`; it holds no
# test logic of its own. --repo names the checkout to test (default: the one the current directory
# is in), so a worktree can be tested without changing directory.
#
# Gate (--gate <set>, koinon-private#512) submits the queue's WorkflowTemplate koinon-gate for one gate set of
# hack/lab/gate-run.sh (go-test, agentd-ginkgo, ...), which replaces the light lane. Parameters
# gate-set, ref (the 40-hex commit), bundle, agent and slot (gate-1..gate-4, the one with no unfinished
# run: label $KOINON_LAB_DOMAIN/lane=gate). The template sets the lane, target and agent labels
# itself (agent from the agent parameter); the submit passes none. Every
# slot busy, or the run still Pending after 120 s (KOINON_E2E_GATE_PENDING: the slot mutex, the
# namespace limit or a ResourceQuota), deletes the run and exits 75 so the caller runs the gate
# locally. A Pending run is deleted, never stopped: a stopped run turns Failed and the lab's ntfy
# sensor pushes a false "Failed". Only a Running run is stopped (Ctrl-C). The log is followed as it is written, then $OUT (result.txt, driver.log, reports) is
# fetched. A set the template does not accept, or a 4xx answer to the submit, exits 2.
#
# Exit status (every lane; a caller tells "the e2e failed" from "no lab" by it, koinon-private#547):
#   0   the run passed
#   1   the run failed: the target's own status, or 1 when that status would collide with a code
#       below (make reports a failed target as 2). The gate lane passes the gate's own status through
#   2   usage, or the lab rejected the request (4xx)
#   75  the lab cannot run it now (unreachable, a failed upload or submit, every slot busy, a gate run
#       still Pending after 120 s, or the run ended with no verdict): nothing was decided, fall back
#       (the gate: run it locally; the e2e: the PR's own quickstart, dispatch `gh workflow run e2e-kind.yml --ref <branch>` only to re-run)
#   76  NOT RUN (gate: result.txt verdict=notrun, hack/go-test.sh exited 3, the run proved less than
#       it should, koinon-private#507)
#   78  no lab configured (a setting or the credentials/manifest file is missing): there is no lab to
#       use; run the e2e on your own Docker (--local) or on GitHub
#   79  refused up front: the change touches flavors/ and the lab cannot build a flavor image (nvcr.io
#       is not reachable from it). GitHub's e2e-kind runs it; --allow-flavors when the image is in Zot
#
# Commit status (koinon-private#547): a followed kind e2e run posts `e2e-lab` on the commit it ran (the ref it
# submitted): success on exit 0, failure when the target failed, nothing on a fallback (75), when no lab
# is configured, with --no-wait (a later --follow posts it), or with --no-status / KOINON_E2E_NO_STATUS=1.
# A run that ends Failed with no result.txt (a hang, an OOM, a pod that died first) is a lab fault, exit
# 75: it goes to GitHub and leaves no lab result. --no-status is not remembered by the run: a later
# `--follow` of a --no-wait run posts unless it gets --no-status again.
# The description names the run, the target_url is the run's page on the Argo Server of lab.env. A commit
# that is not on GitHub yet cannot take a status: the script says so and prints the command to post it
# after the push (hack/e2e-status.sh). `hack/pr-wait.sh <PR> --checks --require e2e` accepts it.
# The followed gate run of the set go-test posts `go-test-lab` the same way (success on exit 0, failure when the
# gate failed, nothing on 75/76 or with --no-status, koinon-private#610): ci.yml's go-test job is skipped for a pull request
# head that has a green one (hack/go-test-lab.sh, docs/dev/maintainer-homelab.md). A gate run picks a free
# slot, gate-1..gate-4 with no unfinished gate run (pick_slot), never a fixed one.
#
#   hack/e2e-remote.sh --argo [--lane kind|light] [--repo DIR] [--target T] [--slot S] [--out DIR] [--no-wait] [--no-status] [--allow-flavors] [--dry-run]
#   hack/e2e-remote.sh --argo --gate <set> [--repo DIR] [--out DIR] [--no-wait] [--dry-run]
#   hack/e2e-remote.sh --argo --follow <workflow name> [--out DIR]
#
# Credentials never appear in argv, logs or files: a credentials file (KOINON_E2E_ARGO_CREDS,
# sourced; template hack/lab/creds.example.sh) defines e2e_creds_open, e2e_creds_argo_token (prints a
# bearer token), e2e_creds_s3 (prints ACCESS_KEY_ID:SECRET_KEY for the bundle bucket), optionally
# e2e_creds_results (the same for a read-only key on the results bucket) and e2e_creds_close; the
# values stay in shell variables and reach curl as a config on stdin (-K -).
#
# Environment (or the lab config file, ${XDG_CONFIG_HOME:-~/.config}/koinon/lab.env; the environment wins):
#   KOINON_E2E_ARGO_SERVER     Argo Server URL (required)
#   KOINON_LAB_DOMAIN          the lab's DNS domain (required for --gate): the lane label of the gate runs is
#                              <KOINON_LAB_DOMAIN>/lane. Lab hosts are <name>.apps.<domain>; never tracked
#   KOINON_E2E_BUNDLE_URL      S3 bucket URL for the bundles (required)
#   KOINON_E2E_RESULTS_URL     S3 bucket URL of the run results (required)
#   KOINON_E2E_ARGO_CREDS      credentials file (required)
#   KOINON_E2E_LIGHT_MANIFEST  Workflow manifest of the light lane (required for --lane light)
#   KOINON_E2E_S3_REGION       SigV4 region (us-east-1)
#   KOINON_E2E_ARGO_NAMESPACE  namespace of the template and runs (koinon-e2e)
#   KOINON_E2E_ARGO_TEMPLATE   WorkflowTemplate (koinon-make); parameters target, ref, bundle, agent, slot
#   KOINON_E2E_GATE_TEMPLATE   WorkflowTemplate of --gate (koinon-gate); parameters gate-set, ref, bundle, agent, slot
#   KOINON_E2E_GATE_PENDING    seconds a gate run may stay Pending before it is given up (120)
#   KOINON_E2E_ARGO_POLL       seconds between status polls (30)
#   KOINON_E2E_NO_STATUS       1: do not post the e2e-lab commit status (same as --no-status)
#   KOINON_E2E_ALLOW_FLAVORS   1: run although flavors/ changed (same as --allow-flavors)
#   KOINON_E2E_BASE            the base the flavors check diffs against (origin/main)
#   KOINON_E2E_AGENT           who submits, a label on the run (KOINON_AGENT_SESSION, else $USER)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=hack/lab/config.sh disable=SC1091
. "$here/lab/config.sh"
lab_config_load
SERVER=${KOINON_E2E_ARGO_SERVER:-}
NS=${KOINON_E2E_ARGO_NAMESPACE:-koinon-e2e}
TEMPLATE=${KOINON_E2E_ARGO_TEMPLATE:-koinon-make}
GATE_TEMPLATE=${KOINON_E2E_GATE_TEMPLATE:-koinon-gate}
GATE_PENDING=${KOINON_E2E_GATE_PENDING:-120}
LOG_GRACE=${KOINON_E2E_LOG_GRACE:-30}
BUNDLE_URL=${KOINON_E2E_BUNDLE_URL:-}
RESULTS_URL=${KOINON_E2E_RESULTS_URL:-}
REGION=${KOINON_E2E_S3_REGION:-us-east-1}
CREDS=${KOINON_E2E_ARGO_CREDS:-}
POLL=${KOINON_E2E_ARGO_POLL:-30}
AGENT=${KOINON_E2E_AGENT:-${KOINON_AGENT_SESSION:-${USER:-agent}}}

lane=kind target="" slot="" out=${KOINON_E2E_OUT:-} wait=1 dry=0 follow="" repo="" allow_flavors=0
[[ ${KOINON_E2E_ALLOW_FLAVORS:-} != 1 ]] || allow_flavors=1
while [[ $# -gt 0 ]]; do
  case $1 in
  --argo) ;;
  --lane) lane=${2:?--lane needs kind or light}; shift ;;
  --gate) lane=gate; target=${2:?--gate needs a gate set}; shift ;;
  --repo) repo=${2:?--repo needs a directory}; shift ;;
  --target) target=${2:?--target needs a make target}; shift ;;
  --slot) slot=${2:?--slot needs a slot name}; shift ;;
  --out) out=${2:?--out needs a directory}; shift ;;
  --no-wait) wait=0 ;;
  --no-status) export KOINON_E2E_NO_STATUS=1 ;;
  --allow-flavors) allow_flavors=1 ;;
  --follow) follow=${2:?--follow needs a workflow name}; shift ;;
  --dry-run) dry=1 ;;
  *) echo "e2e-remote --argo: unknown argument $1" >&2; exit 2 ;;
  esac
  shift
done
case $lane in
kind) target=${target:-test-e2e-kind} ;;
light) target=${target:-test-go} ;;
gate) [[ $target =~ ^[a-z0-9-]+$ ]] || { echo "e2e-remote --argo: --gate needs a gate set name (go-test, agentd-ginkgo, ...)" >&2; exit 2; } ;;
*) echo "e2e-remote --argo: --lane is kind or light (or --gate <set>)" >&2; exit 2 ;;
esac
MANIFEST=${KOINON_E2E_LIGHT_MANIFEST:-}
lab_require "e2e-remote --argo" KOINON_E2E_ARGO_SERVER KOINON_E2E_BUNDLE_URL KOINON_E2E_RESULTS_URL KOINON_E2E_ARGO_CREDS
[[ $lane != light ]] || lab_require "e2e-remote --argo --lane light" KOINON_E2E_LIGHT_MANIFEST
[[ $lane != gate ]] || lab_require "e2e-remote --argo --gate" KOINON_LAB_DOMAIN
[[ -r $CREDS ]] || { echo "e2e-remote --argo: credentials file $CREDS is not readable (KOINON_E2E_ARGO_CREDS)" >&2; exit 78; }
[[ $lane != light || -r $MANIFEST ]] || { echo "e2e-remote --argo: manifest $MANIFEST is not readable (KOINON_E2E_LIGHT_MANIFEST)" >&2; exit 78; }
if [[ -n ${SUITE_LABEL:-} ]]; then
  echo "e2e-remote --argo: SUITE_LABEL is not supported by the queue's template; use --local" >&2
  exit 2
fi

# A label value: at most 63 characters of [A-Za-z0-9_.-], starting and ending alphanumeric.
label=$(printf %s "$AGENT" | tr -c 'A-Za-z0-9_.-' '-' | cut -c1-63 | sed 's/^[^A-Za-z0-9]*//; s/[^A-Za-z0-9]*$//')
label=${label:-agent}

status_sha="" token="" s3user="" opened=0 name="" finished=0 log_pid="" cur_phase=""
# shellcheck source=hack/lab/creds.example.sh disable=SC1091
. "$CREDS"

cleanup() {
  stop_log
  if [[ -n $name && $finished == 0 && $wait == 1 ]]; then
    if [[ $cur_phase == Running ]]; then
      echo "e2e-remote --argo: stopping $name" >&2
      api PUT "/api/v1/workflows/$NS/$name/stop" '{}' >/dev/null 2>&1 || true
    else
      # Pending or not yet seen: a stop would turn it Failed (a false push); delete it.
      echo "e2e-remote --argo: deleting $name (not started)" >&2
      api DELETE "/api/v1/workflows/$NS/$name" >/dev/null 2>&1 || true
    fi
  fi
  # A --no-wait run keeps the login: closing it would revoke the token the run was submitted with.
  if [[ $opened == 1 && $wait == 1 ]]; then e2e_creds_close || true; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# lab_down <message>: the lab cannot be reached: exit 75, so the caller falls back (the gate runs
# locally, the e2e on GitHub) and can tell it from a failed run (1).
lab_down() {
  [[ -z ${bundle:-} ]] || rm -f "$bundle"
  echo "e2e-remote --argo: $1" >&2
  if [[ $lane == gate ]]; then echo "e2e-remote --argo: the lab is unreachable; run the gate locally" >&2
  else echo "e2e-remote --argo: the lab is unreachable; wait for the PR's own quickstart (dispatch gh workflow run e2e-kind.yml only to re-run) or use --local" >&2; fi
  exit 75
}

# fail_rc <status>: a failed run's status, kept unless it collides with a reserved code above (make
# reports a failed target as 2); then 1.
fail_rc() {
  case $1 in 0 | 2 | 75 | 76 | 77 | 78 | 79) echo 1 ;; *) echo "$1" ;; esac
}

# post_status <success|failure> <description> [context]: a commit status (e2e-lab by default, go-test-lab
# for the go-test gate, koinon-private#610) on the commit the run tested. A failed post is a warning; the run's own
# result stands.
post_status() {
  local top ctx=${3:-e2e-lab}
  [[ -n ${status_sha:-} ]] || return 0
  top=$(git -C "${repo:-.}" rev-parse --show-toplevel 2>/dev/null || pwd)
  (cd "$top" && KOINON_STATUS_CONTEXT=$ctx "$here/e2e-status.sh" "$status_sha" "$1" "$2" "$SERVER/workflows/$NS/$name") ||
    echo "e2e-remote --argo: the $ctx status was not posted; the run's result stands" >&2
}

open_creds() {
  [[ $opened == 1 ]] && return 0
  e2e_creds_open || lab_down "cannot open the credentials from $CREDS"
  opened=1
  token=$(e2e_creds_argo_token) || token=""
  [[ -n $token ]] || lab_down "no Argo token from $CREDS"
}

# api <method> <path> [json body]: one Argo Server call; prints the response body. The token reaches
# curl as a config on stdin, never in argv.
api() {
  local method=$1 path=$2 body=${3:-}
  local args=(-sS --fail-with-body -X "$method" -H 'Accept: application/json')
  [[ -z $body ]] || args+=(-H 'Content-Type: application/json' --data-binary "$body")
  printf 'header = "Authorization: Bearer %s"\n' "$token" | curl -K - "${args[@]}" "$SERVER$path"
}

# s3_put <file> <object>: upload with SigV4; the key reaches curl on stdin.
s3_put() {
  local code
  [[ -n $s3user ]] || s3user=$(e2e_creds_s3) || s3user=""
  [[ -n $s3user ]] || { echo "e2e-remote --argo: no bundle credentials from $CREDS" >&2; return 1; }
  code=$(printf 'user = "%s"\n' "$s3user" | curl -K - -sS -o /dev/null -w '%{http_code}' \
    --aws-sigv4 "aws:amz:$REGION:s3" -H 'x-amz-content-sha256: UNSIGNED-PAYLOAD' -T "$1" "$BUNDLE_URL/$2")
  [[ $code == 200 ]] || { echo "e2e-remote --argo: bundle upload returned HTTP $code" >&2; return 1; }
}

# slots prints the lane's slot names: the enum of the template's (kind) or the manifest's (light)
# slot parameter; "a b" if the template has none.
slots() {
  local s
  if [[ $lane == gate ]]; then
    s=$(api GET "/api/v1/workflow-templates/$NS/$GATE_TEMPLATE" |
      jq -r '[.spec.arguments.parameters[]? | select(.name == "slot") | .enum[]?] | join(" ")')
    echo "${s:-gate-1 gate-2 gate-3 gate-4}"
    return
  fi
  if [[ $lane == light ]]; then
    jq -r '[.spec.arguments.parameters[] | select(.name == "slot") | .enum[]] | join(" ")' "$MANIFEST"
    return
  fi
  s=$(api GET "/api/v1/workflow-templates/$NS/$TEMPLATE" |
    jq -r '[.spec.arguments.parameters[]? | select(.name == "slot") | .enum[]?] | join(" ")')
  echo "${s:-a b}"
}

# gate_sets prints the gate sets the template accepts (the enum of its gate-set parameter; empty when
# it lists none). Returns non-zero when the template cannot be read.
gate_sets() {
  api GET "/api/v1/workflow-templates/$NS/$GATE_TEMPLATE" |
    jq -r '[.spec.arguments.parameters[]? | select(.name == "gate-set") | .enum[]?] | join(" ")'
}

# gate_submit <json body>: submit the gate run; prints the workflow name. A 4xx answer is a rejected
# request (exit 2, the arguments are wrong); a network failure or a 5xx answer is the lab being
# unavailable (exit 75).
gate_submit() {
  local out code resp
  out=$(mktemp "${TMPDIR:-/tmp}/koinon-e2e-submit.XXXXXX")
  code=$(printf 'header = "Authorization: Bearer %s"\n' "$token" | curl -K - -sS -o "$out" -w '%{http_code}' \
    -X POST -H 'Accept: application/json' -H 'Content-Type: application/json' --data-binary "$1" \
    "$SERVER/api/v1/workflows/$NS/submit") || code=000
  resp=$(cat "$out"); rm -f "$out"
  case $code in
  2??) jq -r '.metadata.name // empty' <<<"$resp" ;;
  4??) echo "e2e-remote --argo: the lab rejected the gate run (HTTP $code): $resp" >&2; exit 2 ;;
  *) echo "e2e-remote --argo: the submit failed (HTTP $code); run the gate locally" >&2; exit 75 ;;
  esac
}

# pick_slot prints the slot with the fewest unfinished runs (the first one on a tie).
pick_slot() {
  local all
  all=$(slots)
  if [[ $lane == gate ]]; then
    # A gate slot is free only with no unfinished run at all: Running, or Pending on the slot mutex,
    # the namespace limit or a ResourceQuota. Prints nothing when every slot is taken.
    api GET "/api/v1/workflows/$NS?listOptions.labelSelector=$KOINON_LAB_DOMAIN/lane=gate,workflows.argoproj.io/completed!=true&fields=items.spec.arguments.parameters" |
      jq -r --arg all "$all" '
        ($all | split(" ")) as $slots
        | [.items[]? | .spec.arguments.parameters[]? | select(.name == "slot") | .value] as $busy
        | [$slots[] | select(. as $s | ([$busy[] | select(. == $s)] | length) == 0)] | .[0] // empty'
    return
  fi
  api GET "/api/v1/workflows/$NS?listOptions.labelSelector=workflows.argoproj.io/completed!=true&fields=items.spec.arguments.parameters" |
    jq -r --arg all "$all" '
      ($all | split(" ")) as $slots
      | [.items[]? | .spec.arguments.parameters[]? | select(.name == "slot") | .value] as $busy
      | [$slots[] as $s | {s: $s, n: ([$busy[] | select(. == $s)] | length)}]
      | min_by(.n) | .s'
}

# stage_lines prints the main container's "== " lines of the last <seconds>.
stage_lines() {
  api GET "/api/v1/workflows/$NS/$name/log?logOptions.container=main&logOptions.sinceSeconds=$1" 2>/dev/null |
    jq -r '.result.content? // empty' 2>/dev/null | grep -E '^== ' || true
}

# fetch_results <dir>: the run's results archive (timings, driver.log, diagnostics, result.txt).
# With a results key (e2e_creds_results) it is read from the results bucket directly: in client auth
# mode the Argo Server reads the artifact repository's Secret as the caller, which a submit token
# must not be able to. Without one, through the server's /artifact-files.
fetch_results() {
  local dir=$1 status node key tgz reader="" ok=1
  mkdir -p "$dir"
  # A reused --out may hold an earlier run's verdict: never let it stand in for this run's.
  rm -rf "$dir/result.txt" "$dir/out"
  status=$(api GET "/api/v1/workflows/$NS/$name")
  node=$(jq -r '[.status.nodes[]? | select(.type == "Pod")][0].id // empty' <<<"$status")
  [[ -n $node ]] || { echo "e2e-remote --argo: the run has no pod; no results" >&2; return 0; }
  key=$(jq -r --arg n "$node" '.status.nodes[$n].outputs.artifacts[]? | select(.name == "results") | .s3.key // empty' <<<"$status")
  tgz=$(mktemp "${TMPDIR:-/tmp}/koinon-e2e-results.XXXXXX")
  if [[ -n $key ]] && declare -F e2e_creds_results >/dev/null; then
    reader=$(e2e_creds_results)
    printf 'user = "%s"\n' "$reader" | curl -K - -sS --fail -o "$tgz" \
      --aws-sigv4 "aws:amz:$REGION:s3" -H 'x-amz-content-sha256: UNSIGNED-PAYLOAD' "$RESULTS_URL/$key" || ok=0
  else
    printf 'header = "Authorization: Bearer %s"\n' "$token" |
      curl -K - -sS --fail -o "$tgz" "$SERVER/artifact-files/$NS/workflows/$name/$node/outputs/results" || ok=0
  fi
  if [[ $ok == 1 ]]; then
    tar -xzf "$tgz" -C "$dir" 2>/dev/null || tar -xf "$tgz" -C "$dir" 2>/dev/null ||
      echo "e2e-remote --argo: could not unpack the results archive" >&2
    # The archive holds the out directory itself; flatten it.
    if [[ -d $dir/out ]]; then mv "$dir"/out/* "$dir"/ 2>/dev/null || true; rmdir "$dir/out" 2>/dev/null || true; fi
    # Trust only a verdict that names this run's commit (a result.txt without a ref= field is taken as is).
    local res_ref
    res_ref=$(sed -n 's/.* ref=\([0-9a-f]\{7,\}\).*/\1/p' "$dir/result.txt" 2>/dev/null | tail -1 || true)
    if [[ -n $res_ref && -n ${status_sha:-} && $status_sha != "$res_ref"* ]]; then
      echo "e2e-remote --argo: result.txt names ref $res_ref, not ${status_sha:0:12}; ignoring it" >&2
      rm -f "$dir/result.txt"
    fi
  else
    echo "e2e-remote --argo: no results archive (the run may have ended before it wrote one)" >&2
  fi
  rm -f "$tgz"
}

# gate_exit <phase> <result.txt>: the lab-gate exit status from the gate's verdict line
# (hack/lab/gate-run.sh): pass 0, fail its rc (1 when it has none), notrun 76. Without a verdict
# (a commit whose template fell back to light-run.sh, or a pod that died first) the phase decides.
gate_exit() {
  local phase=$1 f=$2 verdict="" grc=""
  verdict=$(sed -n 's/.* verdict=\([a-z]*\).*/\1/p' "$f" 2>/dev/null | tail -1 || true)
  grc=$(sed -n 's/.* rc=\([0-9]*\) .*/\1/p' "$f" 2>/dev/null | tail -1 || true)
  case $verdict in
  pass) return 0 ;;
  notrun) echo "e2e-remote --argo: NOT RUN: the gate skipped what it expected to run (result.txt: $(tr -d '\n' <"$f"))" >&2; return 76 ;;
  fail) [[ -n $grc && $grc != 0 ]] && return "$(fail_rc "$grc")"; return 1 ;;
  esac
  [[ $phase == Succeeded ]] && return 0
  [[ -n $grc && $grc != 0 ]] && return "$(fail_rc "$grc")"
  return 1
}

# follow_run: poll until the run ends; print phase changes and stage lines; fetch the results.
# follow_log: stream the main container's log as it is written (gate lane), in the background.
follow_log() {
  [[ -z $log_pid ]] || return 0
  {
    printf 'header = "Authorization: Bearer %s"\n' "$token" |
      curl -K - -sSN "$SERVER/api/v1/workflows/$NS/$name/log?logOptions.container=main&logOptions.follow=true" 2>/dev/null |
      jq -r --unbuffered '.result.content? // empty' 2>/dev/null || true
  } &
  log_pid=$!
}

# stop_log: end the log stream this script started: the subshell and its curl and jq children.
stop_log() {
  [[ -n $log_pid ]] || return 0
  pkill -P "$log_pid" 2>/dev/null || true
  kill "$log_pid" 2>/dev/null || true
  log_pid=""
}

# wait_log: let the stream deliver its last lines (it ends with the run), but at most
# KOINON_E2E_LOG_GRACE seconds; then stop_log.
wait_log() {
  local i=0
  while [[ -n $log_pid ]] && kill -0 "$log_pid" 2>/dev/null; do
    if [[ $i -ge $LOG_GRACE ]]; then break; fi
    i=$((i + 1)); sleep 1
  done
  if [[ -n $log_pid ]] && kill -0 "$log_pid" 2>/dev/null; then stop_log; else wait "$log_pid" 2>/dev/null || true; log_pid=""; fi
}

# give_up: delete the run that never started; exit 75, the caller runs the gate locally. No stop first:
# a stopped run turns Failed and the lab's ntfy sensor pushes a false "Failed" for a run that never ran.
give_up() {
  echo "e2e-remote --argo: $name is still Pending after ${GATE_PENDING}s (reason: ${1:-slot}); giving up, run the gate locally" >&2
  if ! api DELETE "/api/v1/workflows/$NS/$name" >/dev/null 2>&1; then
    echo "e2e-remote --argo: WARNING could not delete $name in namespace $NS; it is still queued and may start later, delete it by hand" >&2
  fi
  finished=1
  exit 75
}

follow_run() {
  local phase="" last="" pods="" msg seen="" line rc pending_since=$SECONDS
  echo "== following $name ($SERVER/workflows/$NS/$name)"
  while :; do
    # A failed poll (a network blip) keeps the last phase; the next poll tries again.
    # pods is why a pod waits for capacity: "quota" (the ResourceQuota refused the pod: the node
    # stays Pending with "exceeded quota", no pod exists), "unschedulable" (the pod exists, PodScheduled
    # False: Argo copies "Unschedulable: 0/N nodes are available: Insufficient cpu" into the node), "none"
    # otherwise. A pod that is scheduled and still starting (ContainerCreating, an image pull) has
    # neither message and is not waiting for capacity.
    read -r phase pods msg < <(api GET "/api/v1/workflows/$NS/$name?fields=status.phase,status.message,status.nodes" 2>/dev/null |
      jq -r '[.status.nodes[]? | select(.type == "Pod" and .phase == "Pending") | .message // ""] as $p
        | (($p + [.status.message // ""]) | map(ascii_downcase)) as $m
        | "\(.status.phase // "Pending") \(if ((.status.message // "") | test("waiting for .*lock"; "i")) then "none" elif ($m | any(test("exceeded quota|is forbidden"))) then "quota" elif ($m | any(test("unschedulable|insufficient|nodes are available"))) then "unschedulable" elif ($m | any(test("postponed|throttled|parallelism"))) then "namespace-limit" else "none" end) \(.status.message // "")"' 2>/dev/null) || { phase=${last:-Pending}; pods=none; msg=""; }
    cur_phase=$phase
    if [[ $phase != "$last" ]]; then echo "== $(date -u +%H:%M:%SZ) $phase${msg:+: $msg}"; last=$phase; fi
    if [[ $lane == gate ]]; then
      local stuck=0
      local why=slot
      if [[ $pods != none ]]; then stuck=1; why=$pods; elif [[ $phase == Pending ]]; then stuck=1; fi
      if [[ $stuck == 0 ]]; then pending_since=$SECONDS; fi
      if [[ $stuck == 1 && $((SECONDS - pending_since)) -ge $GATE_PENDING ]]; then give_up "$why"; fi
      [[ $stuck == 1 ]] || follow_log
    elif [[ $phase == Running ]]; then
      while IFS= read -r line; do
        case $seen in *"|$line|"*) ;; *) echo "$line"; seen+="|$line|" ;; esac
      done < <(stage_lines $((POLL + 30)))
    fi
    case $phase in Succeeded | Failed | Error) break ;; esac
    sleep "$POLL"
  done
  finished=1
  wait_log # the stream ends with the run: its last lines
  : "${out:=$(git rev-parse --show-toplevel)/.e2e-out/$name}"
  fetch_results "$out"
  echo
  echo "results in $out"
  [[ -f $out/quickstart-timings.md ]] && cat "$out/quickstart-timings.md"
  [[ -d $out/diagnostics ]] && echo "diagnostics in $out/diagnostics"
  rc=0
  if [[ $lane == gate ]]; then
    gate_exit "$phase" "$out/result.txt" || rc=$?
    # The go-test gate leaves the commit status go-test-lab (koinon-private#610): ci.yml's go-test job is satisfied
    # by a green one on the pull request head. Only a verdict posts: pass (0) or the gate's own
    # failure; NOT RUN (76) and the fallbacks (75) leave nothing, so CI runs go-test itself.
    if [[ $target == go-test ]]; then
      case $rc in
      0) post_status success "$name: go-test passed on the lab" go-test-lab ;;
      75 | 76 | 78 | 79) ;;
      *) post_status failure "$name: go-test failed on the lab (exit $rc)" go-test-lab ;;
      esac
    fi
    echo "gate ($target) $phase, exit status $rc"
    return "$rc"
  fi
  local verdict=yes
  if [[ $phase != Succeeded ]]; then
    rc=$(sed -n 's/.* rc=\([0-9]*\) .*/\1/p' "$out/result.txt" 2>/dev/null || true)
    if [[ $rc == 0 ]]; then
      # The target finished and its results were archived, then the pod ended badly: the lab has
      # seen the Kata VM's agent die after a run, reporting 255 for every container. result.txt is
      # the target's own verdict.
      echo "note: the run ended $phase after its target passed (result.txt: rc=0); counting it as passed" >&2
    else
      if [[ -z $rc ]]; then verdict=no; rc=1; else rc=$(fail_rc "$rc"); fi
    fi
  fi
  if [[ $verdict == no ]]; then
    # The run ended without the target's verdict (the pod died first): nothing was decided.
    echo "e2e ($target) $phase with no result.txt: a lab fault, not a verdict; falling back (exit 75)" >&2
    return 75
  fi
  if [[ $lane == kind && $target == test-e2e-kind ]]; then
    if [[ $rc == 0 ]]; then
      post_status success "$name: e2e passed in $(sed -n 's/.* seconds=\([0-9]*\).*/\1/p' "$out/result.txt" 2>/dev/null | tail -1)s"
    else
      post_status failure "$name: e2e failed (exit $rc)"
    fi
  fi
  echo "e2e ($target) $phase, exit status $rc"
  return "$rc"
}

if [[ -n $follow ]]; then
  name=$follow
  [[ $dry == 0 ]] || { echo "+ follow $name"; exit 0; }
  open_creds
  # The run's own target, for the last line (the default is the kind lane's).
  params=$(api GET "/api/v1/workflows/$NS/$name?fields=spec.arguments.parameters" 2>/dev/null || true)
  target=$(jq -r '[.spec.arguments.parameters[]? | select(.name == "target" or .name == "gate-set") | .value][0] // empty' <<<"$params" 2>/dev/null || true)
  # A gate run is recognised by its gate-set parameter.
  [[ $(jq -r '[.spec.arguments.parameters[]? | select(.name == "gate-set")] | length' <<<"$params" 2>/dev/null || echo 0) == 0 ]] || lane=gate
  target=${target:-run}
  status_sha=$(jq -r '[.spec.arguments.parameters[]? | select(.name == "ref") | .value][0] // empty' <<<"$params" 2>/dev/null || true)
  follow_run
  exit
fi

root=$(git -C "${repo:-.}" rev-parse --show-toplevel)
cd "$root"
sha=$(git rev-parse HEAD)

# The kind e2e builds the flavor image; the lab cannot (nvcr.io is fenced), so a change under flavors/
# goes to GitHub's e2e-kind (koinon-private#547). The rule is hack/needs-e2e.sh --flavors. A check that cannot be
# evaluated (no origin/main) lets the run go on: the lab then fails after minutes, not never.
if [[ $lane == kind && $target == test-e2e-kind && $allow_flavors == 0 ]]; then
  frc=0
  fchanged=$("$here/needs-e2e.sh" --flavors "${KOINON_E2E_BASE:-origin/main}" 2>/dev/null) || frc=$?
  case $frc in
  0)
    {
      echo "e2e-remote --argo: refused: this change touches flavors/, and the lab cannot build a flavor image (nvcr.io is not reachable from it)."
      echo "$fchanged"
      echo "  The PR's own run runs the quickstart on GitHub; wait for it (dispatch gh workflow run e2e-kind.yml --ref <branch> only to re-run)   (or your own Docker: hack/e2e-remote.sh --local)."
      echo "  If the flavor image for this change is already in the lab's registry, add --allow-flavors."
    } >&2
    exit 79 ;;
  1) ;;
  *) echo "warning: could not check for flavor changes against ${KOINON_E2E_BASE:-origin/main}; going on" >&2 ;;
  esac
fi
id="$(date -u +%Y%m%d-%H%M%S)-${sha:0:8}-$$"
object="koinon-${sha:0:12}-$id.bundle"
if [[ -n $(git status --porcelain --untracked-files=no) ]]; then
  echo "warning: uncommitted changes are not tested; the e2e runs commit ${sha:0:12}" >&2
fi
if [[ $dry == 1 ]]; then
  echo "+ git bundle of ${sha:0:12} -> $BUNDLE_URL/$object"
  case $lane in light) tpl=inline ;; gate) tpl=$GATE_TEMPLATE ;; *) tpl=$TEMPLATE ;; esac
  echo "+ submit ($lane lane) $NS/$tpl target=$target ref=$sha bundle=$object agent=$label slot=${slot:-<least busy>}"
  [[ $wait == 0 ]] || echo "+ follow the run, results to ${out:-.e2e-out/<run>}"
  exit 0
fi

# The template clones the bundle, which needs a branch: bundle HEAD under a temporary one.
tmpref="refs/heads/e2e-bundle-$id"
bundle=$(mktemp "${TMPDIR:-/tmp}/koinon-e2e-bundle.XXXXXX")
git update-ref "$tmpref" "$sha"
git bundle create -q "$bundle" "$tmpref" || { git update-ref -d "$tmpref"; rm -f "$bundle"; exit 1; }
git update-ref -d "$tmpref"

# Anything that goes wrong reaching the lab leaves the gate lane's caller to run the gate locally
# (75). The gate slot is picked before the upload, so a busy lab costs no bundle.
open_creds
if [[ $lane == gate ]]; then
  accepted=$(gate_sets) || { rm -f "$bundle"; echo "e2e-remote --argo: the lab is unreachable; run the gate locally" >&2; exit 75; }
  if [[ -n $accepted && " $accepted " != *" $target "* ]]; then
    rm -f "$bundle"
    echo "e2e-remote --argo: the lab's $GATE_TEMPLATE does not accept gate set '$target'; it accepts: $accepted (run the others with hack/lab/gate-run.sh)" >&2
    exit 2
  fi
  [[ -n $slot ]] || slot=$(pick_slot) || { rm -f "$bundle"; echo "e2e-remote --argo: the lab is unreachable; run the gate locally" >&2; exit 75; }
  [[ -n $slot ]] || { rm -f "$bundle"; echo "e2e-remote --argo: every gate slot is busy; run the gate locally" >&2; exit 75; }
fi
echo "== uploading the bundle of ${sha:0:12} ($(wc -c <"$bundle" | tr -d ' ') bytes)"
s3_put "$bundle" "$object" || { rm -f "$bundle"; echo "e2e-remote --argo: the lab is unreachable; falling back" >&2; exit 75; }
rm -f "$bundle"

[[ $lane == gate ]] || { [[ -n $slot ]] || slot=$(pick_slot) || lab_down "cannot list the lab's runs to pick a slot"; }
if [[ $lane == gate ]]; then
  body=$(jq -nc --arg t "$GATE_TEMPLATE" --arg g "$target" --arg ref "$sha" --arg b "$object" \
    --arg a "$label" --arg s "$slot" '{resourceKind: "WorkflowTemplate", resourceName: $t,
      submitOptions: {parameters: ["gate-set=\($g)", "ref=\($ref)", "bundle=\($b)", "agent=\($a)", "slot=\($s)"]}}')
  name=$(gate_submit "$body") || exit $?
elif [[ $lane == light ]]; then
  # The manifest with its parameters filled in; "//" holds its comments and is not a Workflow field.
  body=$(jq -c --arg target "$target" --arg ref "$sha" --arg b "$object" --arg a "$label" --arg s "$slot" '
    del(."//")
    | .metadata.labels["koinon.dev/session"] = $a
    | .spec.arguments.parameters |= map(.value = ({target: $target, ref: $ref, bundle: $b, agent: $a, slot: $s}[.name]))
    | {workflow: .}' "$MANIFEST")
  name=$(api POST "/api/v1/workflows/$NS" "$body" | jq -r '.metadata.name // empty') || name=""
else
  body=$(jq -nc --arg t "$TEMPLATE" --arg target "$target" --arg ref "$sha" --arg b "$object" \
    --arg a "$label" --arg s "$slot" '{resourceKind: "WorkflowTemplate", resourceName: $t,
      submitOptions: {parameters: ["target=\($target)", "ref=\($ref)", "bundle=\($b)", "agent=\($a)", "slot=\($s)"],
                      labels: "koinon.dev/session=\($a)"}}')
  name=$(api POST "/api/v1/workflows/$NS/submit" "$body" | jq -r '.metadata.name // empty') || name=""
fi
[[ -n $name ]] || lab_down "the submit failed or returned no workflow name"
status_sha=$sha
echo "== submitted $name: $target at ${sha:0:12}, slot $slot"

if [[ $wait == 0 ]]; then
  echo "follow it with: hack/e2e-remote.sh --argo --follow $name"
  finished=1
  exit 0
fi
follow_run
