#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# A pinned tool or manifest from a GitHub release must come through hack/fetch-pinned.sh (retry,
# sha256 check) or the composite action .github/actions/fetch-pinned (the same plus a cache), never
# through a bare curl or wget (#431: releases/download answers 504 now and then). This fails on a
# curl or wget whose command (backslash continuations joined) names `releases/download`, in the
# workflows, the composite actions, the top-level hack/*.sh scripts and test/**/*.sh. The spike
# scripts under hack/spike are local experiments and out of scope.
# `hack/pinned-download-lint.sh --self-test` checks the checker against bad input.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)

# check FILE: one line per violation.
check() {
	awk '
		function flush() {
			if (cmd ~ /(^|[^A-Za-z0-9_.\/-])(curl|wget)[ \t]/ && cmd ~ /releases\/download/)
				printf "%s:%d: curl/wget of releases/download; use hack/fetch-pinned.sh or .github/actions/fetch-pinned\n", FILENAME, start
			cmd = ""
		}
		/^[ \t]*#/ { next }
		{
			if (cmd == "") start = FNR
			line = $0
			cont = (line ~ /\\[ \t]*$/)
			sub(/\\[ \t]*$/, "", line)
			cmd = (cmd == "" ? "" : cmd " ") line
			if (!cont) flush()
		}
		END { flush() }
	' "$1"
}

self_test() {
	local tmp
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/pinned-download-lint.XXXXXX")
	trap 'rm -rf "$tmp"' RETURN
	printf 'run: |\n  curl -fsSLo x "https://github.com/a/b/releases/download/v1/x"\n' >"$tmp/bare"
	printf 'run: |\n  curl -fsSL -o x \\\n    "https://github.com/a/b/releases/download/v1/x"\n' >"$tmp/cont"
	printf 'run: |\n  wget https://github.com/a/b/releases/download/v1/x\n' >"$tmp/wget"
	# shellcheck disable=SC2016
	printf 'AS_URL="https://github.com/a/b/releases/download/v1/x"\nhack/fetch-pinned.sh "$AS_URL" abc out\n' >"$tmp/good"
	printf '# curl https://github.com/a/b/releases/download/v1/x\ncurl -fsSL https://example.com/x\n' >"$tmp/comment"
	[ "$(check "$tmp/bare" | wc -l)" -eq 1 ] || { echo "self-test: bare curl not flagged" >&2; exit 1; }
	[ "$(check "$tmp/cont" | wc -l)" -eq 1 ] || { echo "self-test: curl with continuation not flagged" >&2; exit 1; }
	[ "$(check "$tmp/wget" | wc -l)" -eq 1 ] || { echo "self-test: wget not flagged" >&2; exit 1; }
	[ -z "$(check "$tmp/good")" ] || { echo "self-test: helper use flagged" >&2; exit 1; }
	[ -z "$(check "$tmp/comment")" ] || { echo "self-test: comment flagged" >&2; exit 1; }
	echo "pinned-download-lint self-test: ok"
}

# The checker is proven on bad input before it judges the tree.
self_test
[ "${1:-}" != --self-test ] || exit 0

cd "$root"
n=0
bad=0
while IFS= read -r f; do
	case $f in hack/pinned-download-lint.sh | hack/spike/*) continue ;; esac
	n=$((n + 1))
	out=$(check "$f")
	if [ -n "$out" ]; then echo "$out" >&2; bad=1; fi
done < <(git ls-files -z -- '.github/workflows/*.yml' '.github/actions/*/action.yml' 'hack/*.sh' 'test/*.sh' | tr '\0' '\n')
[ "$n" -gt 0 ] || { echo "pinned-download-lint: found no files" >&2; exit 1; }
[ "$bad" -eq 0 ] || exit 1
echo "pinned-download-lint: ok ($n files)"
