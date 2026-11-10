#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests (no network) of expand_vars in hack/dep-age-check.sh: the version a file keeps in a
# variable is what gets dated. Covers the shell, Dockerfile and YAML `NAME: value` forms (plain,
# quoted, trailing comment, list item), and that a name defined with two different values or not at
# all is left unexpanded (so dating it fails closed) instead of the first definition winning (old issue 549).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/dep-age-check-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

# Only the function: the script itself needs git and the network.
eval "$(sed -n '/^expand_vars() {/,/^}/p; /^pm_pin() {/,/^}/p; /^dockerfile_specs=/p' "$repo/hack/dep-age-check.sh")"

fail=0
# The URLs are assembled from parts so that no release-asset URL literal appears in this file:
# dep-age-check scans every changed file, this one included, and would try to date a fixture.
scheme="https:/" host="github.com" path="o/r/releases/download"
# shellcheck disable=SC2016 # a literal ${NAME} for expand_vars
url="$scheme/$host/$path/"'v${TOOL_VERSION}/x'
want() { # want <name> <file content> <expected url>
	local f="$tmp/f" got
	printf '%s\n' "$2" >"$f"
	got=$(expand_vars "$f" "$url")
	if [[ $got == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$got', want '$3'"; fail=1; fi
}
expanded="$scheme/$host/$path/v1.2.3/x"

want "shell NAME=value" 'TOOL_VERSION=1.2.3' "$expanded"
want "Dockerfile ARG" 'ARG TOOL_VERSION=1.2.3' "$expanded"
want "YAML plain" '  TOOL_VERSION: 1.2.3' "$expanded"
want "YAML double quoted" '  TOOL_VERSION: "1.2.3"' "$expanded"
want "YAML single quoted" "  TOOL_VERSION: '1.2.3'" "$expanded"
want "YAML trailing comment" '  TOOL_VERSION: 1.2.3 # newest at least 3 days old' "$expanded"
want "YAML list item" '      - TOOL_VERSION: 1.2.3' "$expanded"
want "the same value twice is fine" 'TOOL_VERSION=1.2.3
  TOOL_VERSION: 1.2.3' "$expanded"
want "two different values stay unexpanded" 'TOOL_VERSION=1.2.3
  TOOL_VERSION: 9.9.9' "$url"
want "an undefined name stays unexpanded" 'OTHER_VERSION=1.2.3' "$url"
want "a longer name is not the name" 'MY_TOOL_VERSION: 9.9.9' "$url"

pins() { # pins <name> <line> <expected "package version" or empty>
	local got
	got=$(pm_pin "$2")
	if [[ $got == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$got', want '$3'"; fail=1; fi
}
pins "packageManager pin" '  "packageManager": "pnpm@12.18.0",' "pnpm 12.18.0"
pins "packageManager pin with hash" '  "packageManager": "pnpm@12.18.0+sha512.abc"' "pnpm 12.18.0"
pins "Dockerfile PNPM_VERSION" 'ARG PNPM_VERSION=12.18.0' "pnpm 12.18.0"
pins "other ARG is not a pin" 'ARG NODE_VERSION=24.1.0' ""
pins "unrelated JSON line" '  "version": "0.0.0",' ""

# The Dockerfile pathspecs must select every Dockerfile, named ones included.
# shellcheck disable=SC2154 # defined by the eval above
selected=$(git -C "$repo" ls-files -- "${dockerfile_specs[@]}")
for want_file in hack/e2e-toolchain/Dockerfile hack/e2e-toolchain/web.Dockerfile; do
	if grep -qxF "$want_file" <<<"$selected"; then echo "ok   dockerfile specs select $want_file"; else echo "FAIL dockerfile specs miss $want_file"; fail=1; fi
done

[[ $fail -eq 0 ]] && echo "dep-age-check tests passed"
exit "$fail"
