#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints name=true|false lines (for $GITHUB_OUTPUT) saying which CI jobs a change can affect.
# With no base ref (a push to main) every job runs. Usage: hack/ci-changes.sh [base-ref]
set -euo pipefail

cd "$(dirname "$0")/.."

jobs=(go web generate chart chartsmoke docs escape flavors)
base=${1:-}
if [[ -z $base ]]; then
	for j in "${jobs[@]}"; do echo "$j=true"; done
	exit 0
fi

files=$(git diff --name-only "$(git merge-base "$base" HEAD)" HEAD)

# Changes to CI itself, the Makefile or the Go module run everything.
everything='^(\.github/workflows/ci\.yml|hack/ci-changes\.sh|Makefile|go\.mod|go\.sum)$'

# match <extended regexp>: some changed file matches.
match() { grep -Eq -- "$1" <<<"$files"; }

# pattern <job>: the paths that can affect the job.
pattern() {
	case $1 in
	web) echo '^(web/|api/proto/)' ;;
	generate) echo '^(api/proto/|internal/db/(queries|migrations)/|sqlc\.yaml|buf\.|internal/gen/|web/src/gen/)' ;;
	chart) echo '^(charts/|hack/chart-check\.sh|hack/chartlint/)' ;;
	# The binaries the chart starts, their flags and everything they import (the Go packages
	# outside internal/ and cmd/) count too.
	chartsmoke) echo '^(charts/|cmd/|internal/|pricing/|connectors/|templates/|api/|web/|test/chartsmoke/|go\.(mod|sum))' ;;
	docs) echo '^(docs/|test/e2e/|charts/|web/e2e-real/|hack/docs-|README\.md)' ;;
	escape) echo '^(charts/|test/escape/)' ;;
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

for j in "${jobs[@]}"; do
	if [[ $j == go ]]; then
		echo "go=$go"
	elif match "$everything" || match "$(pattern "$j")"; then
		echo "$j=true"
	else
		echo "$j=false"
	fi
done
