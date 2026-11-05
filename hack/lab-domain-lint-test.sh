#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline test of hack/lab-domain-lint.sh (#693). The "real" lab domain below is a made-up one, built
# from parts so that this file itself names no lab host.

[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/lab-domain-lint-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fails=0
check() { # <name> <cmd...>
	local name=$1
	shift
	if "$@" >/dev/null 2>&1; then echo "ok   $name"; else echo "FAIL $name"; fails=$((fails + 1)); fi
}
not() { ! "$@"; }

dom="lab.""fixture-corp.""com"
r=$tmp/repo
mkdir -p "$r/hack/lab" "$r/docs"
cp "$root/hack/lab-domain-lint.sh" "$r/hack/"
cp "$root/hack/lab/config.sh" "$r/hack/lab/"
git -C "$r" init -q
git -C "$r" config user.email t@example.invalid
git -C "$r" config user.name t
export KOINON_LAB_CONFIG="$tmp/no-such-lab.env" # never the tester's own lab config
unset KOINON_LAB_DOMAIN
lint() { (cd "$r" && hack/lab-domain-lint.sh "$@"); }
commit() { git -C "$r" add -A && git -C "$r" commit -q -m "$1"; }

echo "see zot.apps.lab.example.invalid and ollama.lab.example.invalid, lab.env.example, steps.lab.outputs.x" >"$r/docs/ok.md"
commit clean
check "example domains and lookalikes pass" lint

echo "registry: zot.apps.$dom" >"$r/docs/bad.md"
commit apps-host
check "an apps host under another domain fails" not lint
check "... and names the file, not the host" sh -c "! (cd '$r' && hack/lab-domain-lint.sh 2>&1) | grep -q fixture-corp"
check "a tree-ish argument checks that tree" not lint HEAD
check "... and passes on a clean one" lint HEAD~1

echo "model: ollama.$dom:11434" >"$r/docs/bad.md"
check "an ollama host fails" not lint
echo "label: $dom/lane" >"$r/docs/bad.md"
check "a bare lab label key fails" not lint
echo "ok" >"$r/docs/bad.md"
check "after the fix it passes" lint

dom2="fixture-corp-internal"
echo "something under $dom2 here" >"$r/docs/bad.md"
check "the exact configured domain fails when set" not env KOINON_LAB_DOMAIN="$dom2" bash -c "cd '$r' && hack/lab-domain-lint.sh"
check "... but passes when it is not configured" lint

# The real tree must be clean.
check "this repository names no lab host" "$root/hack/lab-domain-lint.sh"

[[ $fails -eq 0 ]] || { echo "$fails failed" >&2; exit 1; }
echo "all passed"
