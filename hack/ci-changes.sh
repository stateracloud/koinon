#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints name=true|false lines (for $GITHUB_OUTPUT) saying which CI jobs a change can affect.
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

jobs=(go web generate chart chartsmoke docs escape flavors)
base=${1:-}
if [[ -z $base ]]; then
	for j in "${jobs[@]}"; do echo "$j=true"; done
	exit 0
fi

if [[ $base == --stdin ]]; then
	files=$(cat) # the changed paths, one per line (hack/ci-changes-test.sh)
else
	files=$(git diff --name-only "$(git merge-base "$base" HEAD)" HEAD)
fi

# Changes to CI itself, the Makefile or the Go module run everything.
everything='^(\.github/workflows/ci\.yml|hack/ci-changes\.sh|hack/ci-skip-heavy\.sh|Makefile|go\.mod|go\.sum)$'

# match <extended regexp>: some changed file matches.
match() { grep -Eq -- "$1" <<<"$files"; }

# pattern <job>: the paths that can affect the job.
pattern() {
	case $1 in
	web) echo '^(web/|api/proto/)' ;;
	# Every input and output of `make generate` (hack/ci-changes-test.sh derives them from the
	# Makefile and the generator configs and fails when one is missing here): the protos and
	# the vendored OpenShell protos, the OpenAPI spec and its config, the sqlc inputs, the
	# buf and sqlc configs, protoc-gen-es (web/package.json, its lockfile) and the outputs.
	generate) echo '^(api/proto/|third_party/openshell/proto/|third_party/agentgateway/proto/|api/admin/v1/(openapi|oapi-codegen)\.yaml$|internal/adminapi/api\.gen\.go$|internal/db/(queries|migrations|store)/|sqlc\.yaml|buf\.|internal/gen/|web/src/gen/|web/package\.json$|web/pnpm-lock\.yaml$)' ;;
	# chart-check.sh sources hack/versions.env and every hack/chart-check.d/*.sh.
	chart) echo '^(charts/|hack/chart-check\.sh|hack/chart-check\.d/|hack/chartlint/|hack/versions\.env$)' ;;
	# The binaries the chart starts, their flags and everything they import (the Go packages
	# outside internal/ and cmd/) count too. This list is hand-written and partial: the Jobs
	# also read hack/versions.env and other files the chart smoke test names; a push to main
	# runs the job anyway.
	chartsmoke) echo '^(charts/|cmd/|internal/|pricing/|connectors/|templates/|api/|web/|test/chartsmoke/|go\.(mod|sum))' ;;
	# hack/docs-check.sh also reads internal/preflight/preflight.go and hack/versions.env, and
	# runs the tests of internal/declarative. Its repository-link check reads any file of the repo,
	# so its inputs cannot be listed: this pattern is partial, and a push to main runs docs anyway.
	docs) echo '^(docs/|test/e2e/|charts/|web/e2e-real/|hack/docs-|README\.md|internal/preflight/preflight\.go$|internal/declarative/|hack/versions\.env$)' ;;
	# The admission specs (the only ones CI runs) read the chart and the testenv helper only; the
	# reconciler-side cases need kind or the lab (#66), so their packages are not triggers.
	escape) echo '^(charts/|test/escape/|internal/testing/testenv/)' ;;
	# The suite's Go imports are added by flavors_graph below.
	flavors) echo '^(flavors/|internal/(agentd|flavor|acp)/|test/flavors/|hack/flavor-tag\.sh|templates/)' ;;
	esac
}

# go: any change that is not documentation (*.md, docs/) or a non-Go file of the web app.
go=false
if match "$everything"; then
	go=true
else
	while IFS= read -r f; do
		case $f in
		*.go) go=true ;;
		*.md | docs/* | web/*) ;;
		*) go=true ;;
		esac
	done <<<"$files"
fi

# flavors_graph: a changed file lies in (or below, for embedded files) the directory of a package
# the flavors suite builds. Derived, so a new import of the suite is covered without editing a
# list. If `go list` fails (no toolchain) the job runs: fail closed.
flavors_graph() {
	local dirs d f
	dirs=$(flavors_dirs) || return 0
	while IFS= read -r f; do
		d=${f%/*}
		[[ $d != "$f" ]] || continue # a file in the repository root
		while [[ -n $d ]]; do
			grep -qxF -- "$d" <<<"$dirs" && return 0
			[[ $d == */* ]] || break
			d=${d%/*}
		done
	done <<<"$files"
	return 1
}

for j in "${jobs[@]}"; do
	if [[ $j == go ]]; then
		echo "go=$go"
	elif match "$everything" || match "$(pattern "$j")"; then
		echo "$j=true"
	elif [[ $j == flavors ]] && flavors_graph; then
		echo "$j=true"
	else
		echo "$j=false"
	fi
done
