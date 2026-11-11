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

# --- The rule ------------------------------------------------------------------------------------
#
# The patterns (needed, forced, ignored) and the BINARIES test/e2e/run.sh builds are in
# hack/paths.rules, the one source shared with hack/ci-changes.sh and hack/preflight.sh; the
# comments there say why each exists. A path is relevant when it matches e2e-forced, or matches
# e2e-needed and no e2e-ignored; failing both, when its directory (or an ancestor, for embedded
# files) is a package directory in the build graph of a binary (`go list -deps`, this module only).
# hack/needs-e2e-test.sh checks the binaries against images.sh's `ko build` line.
# shellcheck source=/dev/null
. "$(dirname "${BASH_SOURCE[0]}")/paths-lib.sh" || exit 2
paths_load || exit 2
mapfile -t BINARIES < <(paths_raw e2e-binary -)
# --------------------------------------------------------------------------------------------------

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
in_build_graph() { paths_in_graph "$DEPDIRS" "$1"; }

classify() { # reads paths on stdin; prints the relevant ones
  local p
  while IFS= read -r p; do
    [[ -n $p ]] || continue
    if paths_match e2e-forced - "$p"; then echo "$p"; continue; fi
    [[ $forced_only == 1 ]] && continue
    if paths_match e2e-ignored - "$p"; then continue; fi
    if paths_match e2e-needed - "$p"; then echo "$p"; continue; fi
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
