#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Does this change need the kind e2e (test/e2e/run.sh)? One source of truth for the e2e-kind
# workflow's `decide` job, for AGENTS.md's pre-push rule and for hack/e2e-remote.sh.
#
#   hack/needs-e2e.sh [base]     base defaults to origin/main; the changed files are
#                                `git diff --name-only $(git merge-base base HEAD) HEAD`
#   hack/needs-e2e.sh --stdin    read the changed paths from stdin, one per line (tests)
#
# Exit 0 and print the paths that matter when the e2e is needed; exit 1 and print "not needed"
# otherwise. Exit 2 on a usage or git error.
set -euo pipefail

# --- The rule (the only place to change it) -------------------------------------------------------
#
# The e2e is needed for changes whose behaviour only a full install observes: startup, flags and
# environment, chart objects including Jobs, the schema, cross-service flows, the harness, and the
# e2e itself (#281 changed branch naming in githubtools; #284 a startup flag the chart's migrate Job
# lacked). Unit and package tests cannot see those.
#
# A path is relevant when it matches FORCED, or matches NEEDED and none of IGNORED. FORCED beats
# IGNORED: the e2e runs the quickstart's shell blocks, so the quickstart page is an input although
# it is Markdown, and the e2e's own test files are the e2e, not "Go tests outside it".

# Inputs of test/e2e/run.sh. Derive the hack/ entries by reading run.sh (hack/needs-e2e-test.sh
# fails when run.sh calls a hack/ script that no pattern matches).
NEEDED=(
  '^cmd/'
  '^charts/'
  '^internal/db/migrations/'
  '^internal/(agentd|reconciler|services|api|execution|gateway|ledger|githubtools|tracing)/'
  '^flavors/'
  '^test/fakemodel/'
  '^go\.(mod|sum)$'          # dependency changes reach the binaries
  '^\.ko\.yaml$'            # how ko builds the six images
  '^release/versions\.yaml$' # the chart's image pins, if present
)
FORCED=(
  '^test/e2e/'              # the driver, run.sh, kind/ (up.sh, fakes, values)
  '^\.github/workflows/e2e-kind\.yml$'
  '^hack/docs-e1\.sh$'      # run.sh runs the quickstart blocks through it
  '^docs/site/src/content/docs/quickstart\.md$'
)
IGNORED=(
  '\.md$'
  '^docs/'
  '^web/'                   # Playwright covers the web app
  '^hack/'                  # scripts the e2e does not call (see FORCED)
  '_test\.go$'              # Go tests outside test/e2e (those are FORCED)
)
# --------------------------------------------------------------------------------------------------

any_match() { # <path> <pattern>...
  local p=$1 re
  shift
  for re in "$@"; do [[ $p =~ $re ]] && return 0; done
  return 1
}

classify() { # reads paths on stdin; prints the relevant ones
  local p
  while IFS= read -r p; do
    [[ -n $p ]] || continue
    if any_match "$p" "${FORCED[@]}"; then echo "$p"; continue; fi
    if any_match "$p" "${IGNORED[@]}"; then continue; fi
    if any_match "$p" "${NEEDED[@]}"; then echo "$p"; fi
  done
}

if [[ ${1:-} == --stdin ]]; then
  files=$(cat)
else
  base=${1:-origin/main}
  cd "$(git rev-parse --show-toplevel)"
  mb=$(git merge-base "$base" HEAD 2>/dev/null) || { echo "needs-e2e: no merge base with $base (fetch it?)" >&2; exit 2; }
  files=$(git diff --name-only "$mb" HEAD)
fi

matched=$(classify <<<"$files")
if [[ -n $matched ]]; then
  echo "kind e2e needed; these changed paths matter:"
  echo "  ${matched//$'\n'/$'\n'  }"
  exit 0
fi
echo "kind e2e not needed"
exit 1
