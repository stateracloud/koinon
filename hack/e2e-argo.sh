#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The --argo mode of hack/e2e-remote.sh: run a make target (the kind e2e by default) of the committed
# HEAD on an Argo Workflows queue, the maintainers' home lab by default (its ADR 036). Steps: a git
# bundle of HEAD goes to an S3 bucket, the WorkflowTemplate is submitted through the Argo Server REST
# API (no argo CLI needed) on the slot with the fewest unfinished runs, and the run is followed to the
# end: stage lines while it runs, then the results archive (timings, logs, diagnostics) in --out and
# the run's exit status. Ctrl-C stops the run. --repo names the checkout to test (default: the one
# the current directory is in), so a worktree can be tested without changing directory.
#
#   hack/e2e-remote.sh [--argo] [--repo DIR] [--target T] [--slot S] [--out DIR] [--no-wait] [--dry-run]
#   hack/e2e-remote.sh --argo --follow <workflow name> [--out DIR]
#
# Credentials never appear in argv, logs or files: a credentials file (KOINON_E2E_ARGO_CREDS,
# sourced) defines e2e_creds_open, e2e_creds_argo_token (prints a bearer token),
# e2e_creds_s3 (prints ACCESS_KEY_ID:SECRET_KEY for the bundle bucket), optionally
# e2e_creds_results (the same for a read-only key on the results bucket) and e2e_creds_close; the values stay in shell
# variables and reach curl as a config on stdin (-K -). The default file, hack/e2e-argo-lab-creds.sh,
# reads them from the maintainers' OpenBao and only works for them; use your own file and endpoints
# for your own Argo, or --local.
#
# Environment (defaults: the maintainers' home lab):
#   KOINON_E2E_ARGO_SERVER     Argo Server URL (https://argo.apps.lab.example.invalid)
#   KOINON_E2E_ARGO_NAMESPACE  namespace of the template and runs (koinon-e2e)
#   KOINON_E2E_ARGO_TEMPLATE   WorkflowTemplate (koinon-make); parameters target, ref, bundle, agent, slot
#   KOINON_E2E_BUNDLE_URL      S3 bucket URL for the bundles (https://s3.apps.lab.example.invalid/e2e-bundles)
#   KOINON_E2E_RESULTS_URL     S3 bucket URL of the run results (https://s3.apps.lab.example.invalid/e2e-artifacts)
#   KOINON_E2E_S3_REGION       SigV4 region (garage)
#   KOINON_E2E_ARGO_CREDS      credentials file (hack/e2e-argo-lab-creds.sh)
#   KOINON_E2E_ARGO_POLL       seconds between status polls (30)
#   KOINON_E2E_AGENT           who submits, a label on the run (KOINON_AGENT_SESSION, else $USER)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
SERVER=${KOINON_E2E_ARGO_SERVER:-https://argo.apps.lab.example.invalid}
NS=${KOINON_E2E_ARGO_NAMESPACE:-koinon-e2e}
TEMPLATE=${KOINON_E2E_ARGO_TEMPLATE:-koinon-make}
BUNDLE_URL=${KOINON_E2E_BUNDLE_URL:-https://s3.apps.lab.example.invalid/e2e-bundles}
RESULTS_URL=${KOINON_E2E_RESULTS_URL:-https://s3.apps.lab.example.invalid/e2e-artifacts}
REGION=${KOINON_E2E_S3_REGION:-garage}
CREDS=${KOINON_E2E_ARGO_CREDS:-$here/e2e-argo-lab-creds.sh}
POLL=${KOINON_E2E_ARGO_POLL:-30}
AGENT=${KOINON_E2E_AGENT:-${KOINON_AGENT_SESSION:-${USER:-agent}}}

target=test-e2e-kind slot="" out=${KOINON_E2E_OUT:-} wait=1 dry=0 follow="" repo=""
while [[ $# -gt 0 ]]; do
  case $1 in
  --argo) ;;
  --repo) repo=${2:?--repo needs a directory}; shift ;;
  --target) target=${2:?--target needs a make target}; shift ;;
  --slot) slot=${2:?--slot needs a slot name}; shift ;;
  --out) out=${2:?--out needs a directory}; shift ;;
  --no-wait) wait=0 ;;
  --follow) follow=${2:?--follow needs a workflow name}; shift ;;
  --dry-run) dry=1 ;;
  *) echo "e2e-remote --argo: unknown argument $1" >&2; exit 2 ;;
  esac
  shift
done
if [[ -n ${SUITE_LABEL:-} ]]; then
  echo "e2e-remote --argo: SUITE_LABEL is not supported by the queue's template; use --local" >&2
  exit 2
fi

# A label value: at most 63 characters of [A-Za-z0-9_.-], starting and ending alphanumeric.
label=$(printf %s "$AGENT" | tr -c 'A-Za-z0-9_.-' '-' | cut -c1-63 | sed 's/^[^A-Za-z0-9]*//; s/[^A-Za-z0-9]*$//')
label=${label:-agent}

token="" s3user="" opened=0 name="" finished=0
# shellcheck source=hack/e2e-argo-lab-creds.sh disable=SC1091
. "$CREDS"

cleanup() {
  if [[ -n $name && $finished == 0 && $wait == 1 ]]; then
    echo "e2e-remote --argo: stopping $name" >&2
    api PUT "/api/v1/workflows/$NS/$name/stop" '{}' >/dev/null 2>&1 || true
  fi
  # A --no-wait run keeps the login: closing it would revoke the token the run was submitted with.
  if [[ $opened == 1 && $wait == 1 ]]; then e2e_creds_close || true; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

open_creds() {
  [[ $opened == 1 ]] && return 0
  e2e_creds_open
  opened=1
  token=$(e2e_creds_argo_token)
  [[ -n $token ]] || { echo "e2e-remote --argo: no Argo token from $CREDS" >&2; exit 1; }
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
  [[ -n $s3user ]] || s3user=$(e2e_creds_s3)
  code=$(printf 'user = "%s"\n' "$s3user" | curl -K - -sS -o /dev/null -w '%{http_code}' \
    --aws-sigv4 "aws:amz:$REGION:s3" -H 'x-amz-content-sha256: UNSIGNED-PAYLOAD' -T "$1" "$BUNDLE_URL/$2")
  [[ $code == 200 ]] || { echo "e2e-remote --argo: bundle upload returned HTTP $code" >&2; return 1; }
}

# slots prints the template's slot names (its enum), "a b" if it has none.
slots() {
  local s
  s=$(api GET "/api/v1/workflow-templates/$NS/$TEMPLATE" |
    jq -r '[.spec.arguments.parameters[]? | select(.name == "slot") | .enum[]?] | join(" ")')
  echo "${s:-a b}"
}

# pick_slot prints the slot with the fewest unfinished runs (the first one on a tie).
pick_slot() {
  local all
  all=$(slots)
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
    if [[ -d $dir/out && ! -e $dir/result.txt ]]; then mv "$dir"/out/* "$dir"/ 2>/dev/null || true; rmdir "$dir/out" 2>/dev/null || true; fi
  else
    echo "e2e-remote --argo: no results archive (the run may have ended before it wrote one)" >&2
  fi
  rm -f "$tgz"
}

# follow_run: poll until the run ends; print phase changes and stage lines; fetch the results.
follow_run() {
  local phase="" last="" msg seen="" line rc
  echo "== following $name ($SERVER/workflows/$NS/$name)"
  while :; do
    # A failed poll (a network blip) keeps the last phase; the next poll tries again.
    read -r phase msg < <(api GET "/api/v1/workflows/$NS/$name?fields=status.phase,status.message" 2>/dev/null |
      jq -r '"\(.status.phase // "Pending") \(.status.message // "")"' 2>/dev/null) || { phase=${last:-Pending}; msg=""; }
    if [[ $phase != "$last" ]]; then echo "== $(date -u +%H:%M:%SZ) $phase${msg:+: $msg}"; last=$phase; fi
    if [[ $phase == Running ]]; then
      while IFS= read -r line; do
        case $seen in *"|$line|"*) ;; *) echo "$line"; seen+="|$line|" ;; esac
      done < <(stage_lines $((POLL + 30)))
    fi
    case $phase in Succeeded | Failed | Error) break ;; esac
    sleep "$POLL"
  done
  finished=1
  : "${out:=$(git rev-parse --show-toplevel)/.e2e-out/$name}"
  fetch_results "$out"
  echo
  echo "results in $out"
  [[ -f $out/quickstart-timings.md ]] && cat "$out/quickstart-timings.md"
  [[ -d $out/diagnostics ]] && echo "diagnostics in $out/diagnostics"
  rc=0
  if [[ $phase != Succeeded ]]; then
    rc=$(sed -n 's/.* rc=\([0-9]*\) .*/\1/p' "$out/result.txt" 2>/dev/null || true)
    [[ -n $rc && $rc != 0 ]] || rc=1
  fi
  echo "e2e ($target) $phase, exit status $rc"
  return "$rc"
}

if [[ -n $follow ]]; then
  name=$follow
  [[ $dry == 0 ]] || { echo "+ follow $name"; exit 0; }
  open_creds
  follow_run
  exit
fi

root=$(git -C "${repo:-.}" rev-parse --show-toplevel)
cd "$root"
sha=$(git rev-parse HEAD)
id="$(date -u +%Y%m%d-%H%M%S)-${sha:0:8}-$$"
object="koinon-${sha:0:12}-$id.bundle"
if [[ -n $(git status --porcelain --untracked-files=no) ]]; then
  echo "warning: uncommitted changes are not tested; the e2e runs commit ${sha:0:12}" >&2
fi
if [[ $dry == 1 ]]; then
  echo "+ git bundle of ${sha:0:12} -> $BUNDLE_URL/$object"
  echo "+ submit $NS/$TEMPLATE target=$target ref=$sha bundle=$object agent=$label slot=${slot:-<least busy>}"
  [[ $wait == 0 ]] || echo "+ follow the run, results to ${out:-.e2e-out/<run>}"
  exit 0
fi

# The template clones the bundle, which needs a branch: bundle HEAD under a temporary one.
tmpref="refs/heads/e2e-bundle-$id"
bundle=$(mktemp "${TMPDIR:-/tmp}/koinon-e2e-bundle.XXXXXX")
git update-ref "$tmpref" "$sha"
git bundle create -q "$bundle" "$tmpref" || { git update-ref -d "$tmpref"; rm -f "$bundle"; exit 1; }
git update-ref -d "$tmpref"

open_creds
echo "== uploading the bundle of ${sha:0:12} ($(wc -c <"$bundle" | tr -d ' ') bytes)"
s3_put "$bundle" "$object" || { rm -f "$bundle"; exit 1; }
rm -f "$bundle"

[[ -n $slot ]] || slot=$(pick_slot)
body=$(jq -nc --arg t "$TEMPLATE" --arg target "$target" --arg ref "$sha" --arg b "$object" \
  --arg a "$label" --arg s "$slot" '{resourceKind: "WorkflowTemplate", resourceName: $t,
    submitOptions: {parameters: ["target=\($target)", "ref=\($ref)", "bundle=\($b)", "agent=\($a)", "slot=\($s)"],
                    labels: "koinon.dev/session=\($a)"}}')
name=$(api POST "/api/v1/workflows/$NS/submit" "$body" | jq -r '.metadata.name // empty')
[[ -n $name ]] || { echo "e2e-remote --argo: the submit returned no workflow name" >&2; exit 1; }
echo "== submitted $name: $target at ${sha:0:12}, slot $slot"

if [[ $wait == 0 ]]; then
  echo "follow it with: hack/e2e-remote.sh --argo --follow $name"
  finished=1
  exit 0
fi
follow_run
