#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests of hack/versions-check.sh on a copy of the real tree's inputs: the clean copy passes, and
# each kind of drift (Dockerfile ARG, checksum, setup-helm input, golangci-lint input, a second
# literal in a workflow or a script, the CONTRIBUTING table, a malformed file) fails.
# shellcheck disable=SC2016 # the fixtures edit literal workflow text

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

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
	cp "$repo/hack/versions.env" "$repo/hack/govulncheck.sh" "$w/hack/"
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
subst "$w/.github/workflows/ci.yml" 'version: ${{ env.BUF_VERSION }}' 'version: 1.73.0'
expect_fail "buf-action version drift" "$w" "bufbuild/buf-action"

w=$(fresh)
subst "$w/.github/workflows/ci.yml" '          version: ${{ env.BUF_VERSION }}
' ''
expect_fail "a buf-action without a version" "$w" "without a version: input"

w=$(fresh)
subst "$w/.github/workflows/ci.yml" '          checksum: ${{ env.BUF_SHA256_AMD64 }}
' ''
expect_fail "a buf-action without a checksum" "$w" "without a checksum: input"

w=$(fresh)
subst "$w/.github/workflows/ci.yml" '      - name: load tool versions
        run: hack/versions-env.sh >> "$GITHUB_ENV"
      - uses: bufbuild/buf-action' '      - uses: bufbuild/buf-action'
expect_fail "a buf-action with no versions-env load in its job" "$w" "no earlier step in the job running hack/versions-env.sh"

w=$(fresh)
subst "$w/hack/govulncheck.sh" '"golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}"' 'golang.org/x/vuln/cmd/govulncheck@v1.9.0'
expect_fail "a govulncheck version literal in the wrapper script" "$w" "literal govulncheck/gofumpt"

w=$(fresh)
printf 'go run mvdan.cc/gofumpt@v0.12.0 -l x\n' >"$w/hack/x.sh"
expect_fail "a gofumpt version literal in a script" "$w" "literal govulncheck/gofumpt"

w=$(fresh)
subst "$w/.github/workflows/mirror-agentgateway-test-image.yml" '"github.com/google/go-containerregistry/cmd/crane@${CRANE_VERSION}"' 'github.com/google/go-containerregistry/cmd/crane@v0.22.1'
expect_fail "a crane version literal in a workflow" "$w" "literal govulncheck/gofumpt/crane"

w=$(fresh)
printf 'GOFUMPT_VERSION=v0.1.0\n' >"$w/hack/x.sh"
expect_fail "a script assigns GOFUMPT_VERSION" "$w" "GOFUMPT_VERSION is set outside"

w=$(fresh)
printf 'BAD_VERSION="1.0"\n' >>"$w/hack/versions.env"
expect_fail "a quoted value" "$w" "not <TOOL>_VERSION"

w=$(fresh)
printf 'X_SHA256_AMD64=abc\n' >>"$w/hack/versions.env"
expect_fail "a short checksum" "$w" "not a sha256"

w=$(fresh)
printf 'BASH_ENV=evil.sh\n' >>"$w/hack/versions.env"
expect_fail "a name the workflow loader would pass to \$GITHUB_ENV as BASH_ENV" "$w" "not <TOOL>_VERSION"

# the downloads go through hack/fetch-pinned.sh now, so the rule is fed a line of its own
w=$(fresh)
printf '      - run: echo "${X_SHA256_AMD64} f" | sha256sum -c -\n' >>"$w/.github/workflows/ci.yml"
expect_fail "sha256sum -c line with one space" "$w" "two spaces"

w=$(fresh)
printf '      - run: echo "${X_SHA256_AMD64}f" | sha256sum -c -\n' >>"$w/.github/workflows/g2-kind.yml"
expect_fail "sha256sum -c line with no space" "$w" "two spaces"

w=$(fresh)
subst "$w/CONTRIBUTING.md" "| Helm | v4.3.1 |" "| Helm | v4.3.10 |"
expect_fail "CONTRIBUTING row with a longer version" "$w" "'Helm' row"

# the loader prints no line outside the name rule, even from a hostile file
w=$(fresh)
mkdir -p "$w/hack"
cp "$repo/hack/versions-env.sh" "$w/hack/"
printf 'BASH_ENV=evil.sh\nPATH=/x\nFOO_VERSION=1.0\n' >>"$w/hack/versions.env"
out=$("$w/hack/versions-env.sh")
check "versions-env.sh keeps FOO_VERSION" 0 "$(grep -c '^FOO_VERSION=1.0$' <<<"$out" | sed 's/^1$/0/')"
check "versions-env.sh drops BASH_ENV and PATH" 0 "$(grep -cE '^(BASH_ENV|PATH)=' <<<"$out" || true)"

# dep-age-check fails (no network needed) for a *_VERSION with no tool_repo entry
w=$(mktemp -d "$tmp/dep.XXXXXX") || exit 1
mkdir -p "$w/hack"
cp "$repo/hack/dep-age-check.sh" "$w/hack/"
(
	cd "$w" || exit 1
	git init -q -b main . && git config user.name t && git config user.email t@example.invalid
	echo base >README.txt && git add -A && git commit -q -m base
	printf 'NEWTOOL_VERSION=1.0.0\n' >hack/versions.env && git add -A && git commit -q -m add
)
rc=0
(cd "$w" && hack/dep-age-check.sh HEAD~1 >"$w/out" 2>&1) || rc=$?
check "dep-age-check: an unknown tool fails" 1 "$rc"
grep -q 'NEWTOOL' "$w/out" || { echo "FAIL dep-age output lacks NEWTOOL"; cat "$w/out"; fail=1; }

[[ $fail -eq 0 ]] && echo "versions-check tests passed"
exit "$fail"
