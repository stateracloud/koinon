#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints name=true|false lines (for $GITHUB_OUTPUT) saying which CI jobs a change can affect.
# The path rules are in hack/paths.rules (read through hack/paths-lib.sh); this script adds the
# flavors suite's `go list` build graph.
# With no base ref (a push to main) every job runs. Usage: hack/ci-changes.sh [base-ref]
# `--stdin` in place of the base ref reads the changed paths from stdin (hack/ci-changes-test.sh);
# `--flavors-dirs` prints the directories the flavors suite builds from.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

cd "$(dirname "$0")/.."

# flavors_dirs: the repository-relative directory of every package of this module that the flavors
# suite builds, tests included (`go list -deps -test`, as the flavors job runs it on linux).
# CI_CHANGES_FLAVORS_DIRS (a list of directories) replaces the `go list` call; the test sets it once.
flavors_dirs() {
	local root out dirs
	if [[ -v CI_CHANGES_FLAVORS_DIRS ]]; then
		dirs=$CI_CHANGES_FLAVORS_DIRS
	else
		root=$(pwd -P) # go reports .Dir under the logical or the physical path, so strip both
		out=$(GOOS=linux go list -deps -test -f '{{if not .Standard}}{{.Dir}}{{end}}' ./test/flavors/...) || return 1
		dirs=$(awk -v r="$root/" -v l="$PWD/" 'index($0, r) == 1 { print substr($0, length(r) + 1) } index($0, l) == 1 { print substr($0, length(l) + 1) }' <<<"$out" | sort -u)
	fi
	# The suite's own directory must be in the list; if not, the strip failed (a path outside
	# the checkout, a symlink): fail closed instead of reporting "no dependency changed".
	grep -qxF test/flavors <<<"$dirs" || return 1
	printf '%s\n' "$dirs"
}

if [[ ${1:-} == --flavors-dirs ]]; then
	flavors_dirs
	exit
fi

# shellcheck source=/dev/null
. "$(dirname "$0")/paths-lib.sh" || exit 2
paths_load || exit 2 # the rules (hack/paths.rules) are the only source; an unreadable file is not "no job"
mapfile -t jobs < <(paths_jobs)
base=${1:-}
if [[ -z $base ]]; then
	for j in "${jobs[@]}"; do echo "$j=true"; done
	exit 0
fi

if [[ $base == --stdin ]]; then
	files=$(cat) # the changed paths, one per line (hack/ci-changes-test.sh)
else
	files=$(git diff --name-only --no-renames "$(git merge-base "$base" HEAD)" HEAD)
fi

# A change to CI itself, the Makefile, the Go module or the rules runs everything.
everything=false
# An empty change list is not "nothing changed": a diff that failed or an empty file list runs
# everything (fail closed); the callers that can have no change (ci-skip-heavy.sh) check before.
[[ -n ${files//[$' \t\n']/} ]] || everything=true
while IFS= read -r f; do
	if [[ -n $f ]] && paths_match everything - "$f"; then everything=true; fi
done <<<"$files"

# flavors_graph: a changed file lies in (or below, for embedded files) the directory of a package
# the flavors suite builds. Derived, so a new import of the suite is covered without editing a
# list. If `go list` fails (no toolchain) the job runs: fail closed.
flavors_graph() {
	local dirs f
	dirs=$(flavors_dirs) || return 0
	while IFS= read -r f; do
		[[ -n $f ]] && paths_in_graph "$dirs" "$f" && return 0
	done <<<"$files"
	return 1
}

for j in "${jobs[@]}"; do
	if [[ $everything == true ]] || paths_job_any "$j" <<<"$files"; then
		echo "$j=true"
	elif [[ $j == flavors ]] && flavors_graph; then
		echo "$j=true"
	else
		echo "$j=false"
	fi
done
