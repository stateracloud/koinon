#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests of hack/versions-check.sh on a copy of the real tree's inputs: the clean copy passes, and
# each kind of drift (Dockerfile ARG, checksum, setup-helm input, golangci-lint input, a second
# literal in a workflow or a script, the CONTRIBUTING table, a malformed file) fails.
set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/versions-check-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

fail=0
check() { # check <name> <expected rc> <rc>
	if [[ $2 == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1 (rc $3, want $2)"; fail=1; fi
}

fresh() { # a copy of the inputs, echoes its path
	local w
	w=$(mktemp -d "$tmp/w.XXXXXX") || exit 1
	mkdir -p "$w/hack/e2e-toolchain" "$w/.github/workflows" "$w/test/e2e"
	cp "$repo/hack/versions.env" "$w/hack/"
	cp "$repo/hack/e2e-toolchain/Dockerfile" "$w/hack/e2e-toolchain/"
	cp "$repo"/.github/workflows/*.yml "$w/.github/workflows/"
	cp "$repo/CONTRIBUTING.md" "$w/"
	echo "$w"
}
run() { VERSIONS_CHECK_ROOT=$1 "$repo/hack/versions-check.sh" >"$1/out" 2>&1; }
subst() { # subst <file> <from> <to>: first occurrence, portable (no sed -i)
	local f=$1 content
	content=$(cat "$f")
	[[ $content == *"$2"* ]] || { echo "test setup: '$2' not in $f" >&2; exit 1; }
	printf '%s\n' "${content/"$2"/$3}" >"$f"
}
expect_fail() { # expect_fail <name> <dir> <message fragment>
	local rc=0
	run "$2" || rc=$?
	check "$1: exit 1" 1 "$rc"
	grep -qF -- "$3" "$2/out" || { echo "FAIL $1: output lacks '$3'"; cat "$2/out"; fail=1; }
}

w=$(fresh)
rc=0
run "$w" || rc=$?
check "the repository's own tree is consistent" 0 "$rc"

w=$(fresh)
subst "$w/hack/e2e-toolchain/Dockerfile" "ARG KIND_VERSION=" "ARG KIND_VERSION=x"
expect_fail "Dockerfile kind version drift" "$w" "ARG KIND_VERSION"

w=$(fresh)
subst "$w/hack/e2e-toolchain/Dockerfile" "ARG KO_SHA256_arm64=4" "ARG KO_SHA256_arm64=5"
expect_fail "Dockerfile ko arm64 checksum drift" "$w" "KO_SHA256_arm64"

w=$(fresh)
subst "$w/hack/e2e-toolchain/Dockerfile" "ARG KUBECTL_VERSION=v" "ARG KUBECTL_VERSION=vX"
expect_fail "Dockerfile kubectl drift" "$w" "KUBECTL_VERSION"

w=$(fresh)
subst "$w/.github/workflows/ci.yml" "version: v4.3.1" "version: v4.3.2"
expect_fail "setup-helm version drift" "$w" "azure/setup-helm"

w=$(fresh)
subst "$w/.github/workflows/ci.yml" "version: v2.15.0" "version: v2.15.1"
expect_fail "golangci-lint input drift" "$w" "golangci-lint-action"

w=$(fresh)
subst "$w/.github/workflows/g2-kind.yml" "    steps:" "    env:
      KIND_VERSION: v0.1.0
    steps:"
expect_fail "a workflow env entry repeats a versions.env name" "$w" "KIND_VERSION is set outside"

w=$(fresh)
printf 'KO_VERSION=0.1.0\n' >"$w/test/e2e/x.sh"
expect_fail "a script assigns a versions.env name" "$w" "KO_VERSION is set outside"

w=$(fresh)
subst "$w/CONTRIBUTING.md" "| Helm | v4.3.1 |" "| Helm | v4.3.0 |"
expect_fail "CONTRIBUTING table drift" "$w" "'Helm' row"

w=$(fresh)
printf 'BAD_VERSION="1.0"\n' >>"$w/hack/versions.env"
expect_fail "a quoted value" "$w" "not NAME=value"

w=$(fresh)
printf 'X_SHA256_AMD64=abc\n' >>"$w/hack/versions.env"
expect_fail "a short checksum" "$w" "not a sha256"

[[ $fail -eq 0 ]] && echo "versions-check tests passed"
exit "$fail"
