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
eval "$(sed -n '/^expand_vars() {/,/^}/p; /^pm_pin() {/,/^}/p; /^snapshot_pin() {/,/^}/p; /^check_snapshot() {/,/^}/p; /^age_days() {/,/^}/p; /^report() {/,/^}/p; /^dockerfile_specs=/p' "$repo/hack/dep-age-check.sh")"

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

# The Ubuntu snapshot ARG (issue 33): the line is recognised, and its own timestamp is its age.
snap() { # snap <name> <line> <expected ISO time or empty>
	local got
	got=$(snapshot_pin "$2")
	if [[ $got == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$got', want '$3'"; fail=1; fi
}
snap "snapshot ARG" 'ARG UBUNTU_SNAPSHOT=20261027T000000Z' "2026-10-27T00:00:00Z"
snap "snapshot ARG, indented" '  ARG UBUNTU_SNAPSHOT=20261108T123456Z  ' "2026-11-08T12:34:56Z"
snap "not a snapshot ARG" 'ARG OTHER_SNAPSHOT=20261027T000000Z' ""
snap "malformed snapshot ID" 'ARG UBUNTU_SNAPSHOT=2026-10-27' ""
snap "snapshot ID without the Z" 'ARG UBUNTU_SNAPSHOT=20261027T000000' ""

# report/age_days end to end: an old snapshot passes, one younger than 3 days (or from the future) fails.
fails_is() { # fails_is <n> <ok message> <FAIL message>
	if [[ $fails -eq $1 ]]; then echo "$2"; else echo "$3"; fail=1; fi
}
# shellcheck disable=SC2034 # globals read by the eval-ed report()
fails=0 checked=0 WAIVER='' MIN_AGE_DAYS=3
check_snapshot "2026-10-27T00:00:00Z" 20261027T000000Z >/dev/null
fails_is 0 "ok   an old snapshot passes" "FAIL an old snapshot was rejected"
young=$(date -u -d '1 day ago' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v-1d +%Y-%m-%dT%H:%M:%SZ)
check_snapshot "$young" young >/dev/null
fails_is 1 "ok   a snapshot 1 day old fails" "FAIL a snapshot 1 day old passed"
check_snapshot "2099-01-01T00:00:00Z" future >/dev/null
fails_is 2 "ok   a snapshot from the future fails" "FAIL a future snapshot passed"
# shellcheck disable=SC2034
WAIVER=someone
check_snapshot "$young" young >/dev/null
fails_is 2 "ok   the waiver turns a young snapshot into WAIVED" "FAIL the waiver did not apply"

# The Dockerfile pathspecs must select every Dockerfile, named ones included.
# shellcheck disable=SC2154 # defined by the eval above
selected=$(git -C "$repo" ls-files -- "${dockerfile_specs[@]}")
for want_file in hack/e2e-toolchain/Dockerfile hack/e2e-toolchain/web.Dockerfile; do
	if grep -qxF "$want_file" <<<"$selected"; then echo "ok   dockerfile specs select $want_file"; else echo "FAIL dockerfile specs miss $want_file"; fail=1; fi
done

[[ $fail -eq 0 ]] && echo "dep-age-check tests passed"
exit "$fail"
