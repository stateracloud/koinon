#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Does this change need the kind e2e (test/e2e/run.sh)? One source of truth for the e2e-kind
# workflow's `decide` job, for AGENTS.md's pre-push rule and for hack/e2e-remote.sh.
#
#   hack/needs-e2e.sh [base]     base defaults to origin/main; the changed files are
#                                `git diff --name-only $(git merge-base base HEAD) HEAD`
#   hack/needs-e2e.sh --stdin    read the changed paths from stdin, one per line (tests)
#   --forced-only                only the FORCED set below (the e2e's own files); needs no Go toolchain.
#                                The full rule (no flag) is what makes a pull request run the kind e2e
#                                in its own run, so a fork PR needs no label or lab (issue #148)
#
# Needs the Go toolchain (it asks `go list` for the binaries' dependencies).
# Exit 0 and print the paths that matter when the e2e is needed; exit 1 and print "not needed"
# otherwise. Exit 2 on a usage, git or `go list` error: callers must treat that as "needed".

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

# --- The rule (the only place to change it) -------------------------------------------------------
#
# The e2e is needed for changes whose behaviour only a full install observes: startup, flags and
# environment, chart objects including Jobs, the schema, cross-service flows, the harness, and the
# e2e itself (old issue 281 changed branch naming in githubtools; old issue 284 a startup flag the chart's migrate Job
# lacked). Unit and package tests cannot see those.
#
# Go code is not listed by directory: BINARIES are the commands test/e2e/run.sh builds with ko and
# loads into kind (the three services and the three fakes it deploys; keep in step with its
# `ko build` line, hack/needs-e2e-test.sh checks). A non-test file in the directory of a package in
# their dependency graph (`go list -deps`, this module only), or below it (embedded files), is
# relevant. A new package no binary imports is not.
BINARIES=(
  ./cmd/koinon-api ./cmd/koinon-reconciler ./cmd/koinon-agentd
  ./cmd/koinon-fakemodel ./cmd/koinon-fakegithub ./cmd/koinon-fakemcp
)
#
# A path is relevant when it matches FORCED, or matches NEEDED and none of IGNORED. FORCED beats
# IGNORED: the e2e runs the quickstart's shell blocks, so the quickstart page is an input although
# it is Markdown, and the e2e's own test files are the e2e, not "Go tests outside it".

# Inputs of test/e2e/run.sh. Derive the hack/ entries by reading run.sh (hack/needs-e2e-test.sh
# fails when run.sh calls a hack/ script that no pattern matches).
NEEDED=(
  '^cmd/'                   # backstop: a renamed binary directory breaks `go list` (also fails closed)
  '^vendor/'
  '^charts/'
  '^internal/db/migrations/'
  '^flavors/'
  '^go\.(mod|sum)$'          # dependency changes reach the binaries
  '^go\.work(\.sum)?$'
  '^Makefile$'              # test-e2e-kind is `make test-e2e-kind`
  '^\.ko\.yaml$'            # how ko builds the six images
  '^release/versions\.yaml$' # the chart's image pins, if present
)
FORCED=(
  '^test/e2e/'              # the driver, run.sh, kind/ (up.sh, fakes, values)
  '^test/escape/'           # run.sh calls test/escape/kind.sh through test/e2e/escape-mcp.sh when ESCAPE_MCP=1
  '^\.github/workflows/e2e-kind\.yml$'
  '^flavors/.*\.md$'        # agent prompts and instructions may be Markdown
  '^hack/docs-e1\.sh$'      # run.sh runs the quickstart blocks through it
  '^hack/flavor-tag\.sh$'    # run.sh names the pulled flavor image with it
  '^hack/flavor-base\.sh$'   # run.sh sets the flavor's base image with it (FLAVOR_BASE, #200)
  '^hack/versions\.env$'    # the kind, ko and helm versions e2e-kind.yml installs (old issue 565)
  '^hack/versions-env\.sh$' # the filter that loads them into the workflow's environment
  '^hack/e2e-toolchain/'    # the web toolchain image run.sh uses (Chromium for the browser suite)
  '^docs/site/src/content/docs/quickstart\.md$'
  '^docs/site/src/content/docs/guides/backup-restore\.md$' # test/e2e/backup-restore.sh runs its blocks (#90)
)
IGNORED=(
  '\.md$'
  '^docs/'
  '^web/'                   # Playwright covers the web app
  '^hack/'                  # scripts the e2e does not call (see FORCED)
  '/testdata/'
  '_test\.go$'              # Go tests outside test/e2e (those are FORCED)
)
# --------------------------------------------------------------------------------------------------

any_match() { # <path> <pattern>...
  local p=$1 re
  shift
  for re in "$@"; do [[ $p =~ $re ]] && return 0; done
  return 1
}

# DEPDIRS: the repository-relative directory of every package of this module the binaries build
# (linux, no cgo, as ko builds them) or the driver (test/e2e, with its test dependencies) uses.
# One `go list` each, always run, outside any condition: if it fails (a renamed binary directory,
# no toolchain) the script exits 2, which callers read as "needed".
godirs() { # <go list args...>
  local root out
  root=$(pwd)
  out=$(GOOS=linux CGO_ENABLED=0 go list -deps -f '{{if not .Standard}}{{.Dir}}{{end}}' "$@") ||
    { echo "needs-e2e: go list failed; cannot derive the build graph" >&2; exit 2; }
  awk -v r="$root/" 'index($0, r) == 1 { print substr($0, length(r) + 1) }' <<<"$out"
}

# in_build_graph <path>: the file's directory, or an ancestor of it, is a package dir of a binary.
in_build_graph() {
  local d=${1%/*}
  [[ $d != "$1" ]] || return 1 # a file in the repository root
  while [[ $d != . && -n $d ]]; do
    [[ $DEPDIRS == *$'\n'"$d"$'\n'* ]] && return 0
    [[ $d == */* ]] || break
    d=${d%/*}
  done
  return 1
}

classify() { # reads paths on stdin; prints the relevant ones
  local p
  while IFS= read -r p; do
    [[ -n $p ]] || continue
    if any_match "$p" "${FORCED[@]}"; then echo "$p"; continue; fi
    [[ $forced_only == 1 ]] && continue
    if any_match "$p" "${IGNORED[@]}"; then continue; fi
    if any_match "$p" "${NEEDED[@]}"; then echo "$p"; continue; fi
    if in_build_graph "$p"; then echo "$p"; fi
  done
}

forced_only=0 stdin=0 base=origin/main
for a in "$@"; do
  case $a in
  --forced-only) forced_only=1 ;;
  --stdin) stdin=1 ;;
  -*) echo "needs-e2e: unknown option $a" >&2; exit 2 ;;
  *) base=$a ;;
  esac
done

cd "$(git rev-parse --show-toplevel)"
DEPDIRS=""
if [[ $forced_only == 0 ]]; then
  bin_dirs=$(godirs "${BINARIES[@]}") || exit 2
  drv_dirs=$(godirs -test ./test/e2e) || exit 2
  DEPDIRS=$'\n'$bin_dirs$'\n'$drv_dirs$'\n'
fi
if [[ $stdin == 1 ]]; then
  files=$(cat)
else
  mb=$(git merge-base "$base" HEAD 2>/dev/null) || { echo "needs-e2e: no merge base with $base (fetch it?)" >&2; exit 2; }
  files=$(git diff --name-only --no-renames "$mb" HEAD)
fi

matched=$(classify <<<"$files")
if [[ -n $matched ]]; then
  echo "kind e2e needed; these changed paths matter:"
  echo "  ${matched//$'\n'/$'\n'  }"
  exit 0
fi
echo "kind e2e not needed"
exit 1
