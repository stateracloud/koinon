#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints name=true|false lines (for $GITHUB_OUTPUT) saying which CI jobs a change can affect.
# The path rules are in hack/paths.rules (read through hack/paths-lib.sh); this script adds the
# harnesses and chartsmoke suites' `go list` build graphs.
# With no base ref (a push to main) every job runs. Usage: hack/ci-changes.sh [base-ref]
# `--stdin` in place of the base ref reads the changed paths from stdin (hack/ci-changes-test.sh);
# `--harnesses-dirs` / `--chartsmoke-dirs` print the directories each suite builds from.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

# CI_CHANGES_ROOT (non-empty) is the checkout to read: hack/pr-merge.sh runs the base's copy of this script from a
# temporary directory against the working tree.
cd "${CI_CHANGES_ROOT:-$(dirname "$0")/..}"

# strip_mod_dirs: repository-relative directories from `go list` .Dir lines (logical and physical).
strip_mod_dirs() {
	local root
	root=$(pwd -P) # go reports .Dir under the logical or the physical path, so strip both
	awk -v r="$root/" -v l="$PWD/" 'index($0, r) == 1 { print substr($0, length(r) + 1) } index($0, l) == 1 { print substr($0, length(l) + 1) }' | sort -u
}

# harnesses_dirs: the repository-relative directory of every package of this module that the harnesses
# suite builds, tests included (`go list -deps -test`, as the harnesses job runs it on linux).
# CI_CHANGES_HARNESSS_DIRS (a list of directories) replaces the `go list` call; the test sets it once.
harnesses_dirs() {
	local out dirs
	if [[ -v CI_CHANGES_HARNESSS_DIRS ]]; then
		dirs=$CI_CHANGES_HARNESSS_DIRS
	else
		out=$(GOOS=linux go list -deps -test -f '{{if not .Standard}}{{.Dir}}{{end}}' ./test/harnesses/...) || return 1
		dirs=$(strip_mod_dirs <<<"$out")
	fi
	# The suite's own directory must be in the list; if not, the strip failed (a path outside
	# the checkout, a symlink): fail closed instead of reporting "no dependency changed".
	grep -qxF test/harnesses <<<"$dirs" || return 1
	printf '%s\n' "$dirs"
}

# chartsmoke_dirs: packages the chart smoke suite builds (tests included) plus the three cmd/
# binaries images_test.go compiles with `go build` and no tags. Non-Go inputs the suite reads
# (charts/, hack/versions.env) stay in hack/paths.rules. CI_CHANGES_CHARTSMOKE_DIRS replaces
# the `go list` calls; the test sets it once.
chartsmoke_dirs() {
	local out dirs
	if [[ -v CI_CHANGES_CHARTSMOKE_DIRS ]]; then
		dirs=$CI_CHANGES_CHARTSMOKE_DIRS
	else
		out=$(GOOS=linux go list -deps -test -f '{{if not .Standard}}{{.Dir}}{{end}}' ./test/chartsmoke/...) || return 1
		out+=$'\n'
		out+=$(GOOS=linux go list -deps -f '{{if not .Standard}}{{.Dir}}{{end}}' ./cmd/koinon-api ./cmd/koinon-agentd ./cmd/koinon-reconciler) || return 1
		dirs=$(strip_mod_dirs <<<"$out")
	fi
	grep -qxF test/chartsmoke <<<"$dirs" || return 1
	printf '%s\n' "$dirs"
}

case ${1:-} in
--harnesses-dirs)
	harnesses_dirs
	exit
	;;
--chartsmoke-dirs)
	chartsmoke_dirs
	exit
	;;
esac

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

# A change to CI itself, the Makefile, the Go module or the rules runs everything. With
# CI_CHANGES_IGNORE_PLUMBING=1 the CI-plumbing rule (`everything ci`) is dropped and only the build
# inputs (`everything -`) count: hack/pr-merge.sh --admin asks "is a real harnesses run owed", and a
# change of ci.yml alone cannot get one. An empty list still runs everything.
everything=false
# An empty change list is not "nothing changed": a diff that failed or an empty file list runs
# everything (fail closed); the callers that can have no change (ci-skip-heavy.sh) check before.
[[ -n ${files//[$' \t\n']/} ]] || everything=true
while IFS= read -r f; do
	if [[ -n $f ]] && paths_match everything - "$f"; then everything=true; fi
	if [[ -n $f && ${CI_CHANGES_IGNORE_PLUMBING:-} != 1 ]] && paths_match everything ci "$f"; then everything=true; fi
done <<<"$files"

# suite_graph <dirs-fn> <job>: a changed file lies in (or below, for embedded files) a package
# directory the suite builds, and is not excluded by the job's except/keep rules (so chartsmoke
# still skips web/src and foreign _test.go). Derived, so a new import is covered without editing a
# list. If `go list` fails (no toolchain) the job runs: fail closed.
suite_graph() {
	local dirs_fn=$1 job=$2 dirs f
	dirs=$("$dirs_fn") || return 0
	while IFS= read -r f; do
		[[ -n $f ]] || continue
		paths_in_graph "$dirs" "$f" || continue
		if paths_match except "$job" "$f"; then
			paths_match keep "$job" "$f" && return 0
			continue
		fi
		return 0
	done <<<"$files"
	return 1
}

for j in "${jobs[@]}"; do
	if [[ $everything == true ]] || paths_job_any "$j" <<<"$files"; then
		echo "$j=true"
	elif [[ $j == harnesses ]] && suite_graph harnesses_dirs harnesses; then
		echo "$j=true"
	elif [[ $j == chartsmoke ]] && suite_graph chartsmoke_dirs chartsmoke; then
		echo "$j=true"
	else
		echo "$j=false"
	fi
done
