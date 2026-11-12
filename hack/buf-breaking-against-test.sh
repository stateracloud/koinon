#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline test of hack/buf-breaking-against.sh (#542): a PR behind a main that added a proto
# field must compare against the merge base, not main's tip.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/buf-breaking-against-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

git init -q -b main "$tmp/r"
cd "$tmp/r"
git config user.email "koinon-test@example.com"
git config user.name "koinon-test"
mkdir -p api
printf 'syntax = "proto3";\npackage a.v1;\nmessage A {}\n' >api/a.proto
git add api/a.proto
git commit -qm 'base'
fork=$(git rev-parse HEAD)

git checkout -q -b pr
printf 'pr-only\n' >pr.txt
git add pr.txt
git commit -qm 'pr change'

git checkout -q main
printf 'syntax = "proto3";\npackage a.v1;\nmessage A { string x = 1; }\n' >api/a.proto
git add api/a.proto
git commit -qm 'main adds a proto field'
main=$(git rev-parse HEAD)

git checkout -q pr

against=$("$repo/hack/buf-breaking-against.sh" main)
if [[ $against != "$fork" ]]; then
	echo "FAIL PR against=$against, want merge base $fork"
	exit 1
fi
echo "ok   PR uses the merge base, not main's tip"

mg=$("$repo/hack/buf-breaking-against.sh" "$main" --merge-group)
if [[ $mg != "$main" ]]; then
	echo "FAIL merge_group against=$mg, want $main"
	exit 1
fi
echo "ok   merge_group uses the base tip"

# Sanity: main's tip has the new field; the merge base does not. Comparing the PR with main's tip
# is exactly the false red #542 describes.
if git show "$main:api/a.proto" | grep -q 'string x'; then
	echo "ok   main tip has the new field"
else
	echo "FAIL main tip missing the new field"
	exit 1
fi
if git show "$against:api/a.proto" | grep -q 'string x'; then
	echo "FAIL merge base unexpectedly has the new field"
	exit 1
fi
echo "ok   merge base does not have main's new field"

echo "buf-breaking-against tests passed"
