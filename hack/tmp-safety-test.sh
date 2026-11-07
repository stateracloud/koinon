#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Guards against the old issue 476 class: a hack/ test script whose temp directory failed to be created must
# stop, not run `rm -rf` on the caller's directory (`cd "$(mktemp -d)"` with a failing mktemp cd's
# nowhere, and the EXIT trap then deletes the caller's worktree).
#   1. static: no `cd "$(mktemp` (or pushd) under hack/.
#   2. dynamic: every test script that calls mktemp, run from a canary directory (a bare `.git`
#      file) with a failing mktemp first on PATH and TMPDIR read-only, exits non-zero and leaves the canary.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd -P)
self=$(basename "$0")
fail=0
ok() { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

if grep -rnE '(cd|pushd)[[:space:]]+"?\$\([[:space:]]*mktemp' "$repo/hack" --include='*.sh' | grep -vE ':[0-9]+:[[:space:]]*#' | grep -v "$self:"; then
	bad "static: cd/pushd into an unchecked mktemp result"
else
	ok "static: no cd/pushd into \$(mktemp ...)"
fi

# 2. every script that assigns from mktemp must run under set -e, or an empty result goes on unchecked.
for t in "$repo"/hack/*.sh; do
	[[ $(basename "$t") == "$self" ]] && continue
	if grep -E '^[[:space:]]*[a-zA-Z_]+=\$\([[:space:]]*mktemp' "$t" | grep -vq '||' && ! grep -qE '^set -[a-zA-Z]*e' "$t"; then
		bad "static: $(basename "$t") assigns from mktemp without set -e"
	fi
done

base=$(mktemp -d "${TMPDIR:-/tmp}/tmp-safety.XXXXXX") || exit 1
base=$(cd "$base" && pwd -P)
trap 'chmod -R u+w "$base" 2>/dev/null; rm -rf "$base"' EXIT
mkdir "$base/ro" "$base/bin"
chmod 555 "$base/ro"
# macOS mktemp ignores TMPDIR, so a read-only TMPDIR alone does not make it fail there (and root
# ignores the mode): a stub that fails like a sandboxed mktemp makes the case the same everywhere.
cat >"$base/bin/mktemp" <<'SH'
#!/bin/sh
echo "mktemp: stub: cannot create a temporary path" >&2
exit 1
SH
chmod +x "$base/bin/mktemp"

for t in "$repo"/hack/*-test.sh "$repo"/hack/claim_test.sh; do
	name=$(basename "$t")
	[[ $name == "$self" ]] && continue
	grep -q mktemp "$t" || continue
	canary="$base/canary-$name"
	mkdir "$canary"
	: >"$canary/.git"
	: >"$canary/keep"
	rc=0
	(cd "$canary" && PATH="$base/bin:$PATH" TMPDIR="$base/ro" KUBECONFIG=/dev/null timeout 180 bash "$t" >"$base/$name.log" 2>&1) || rc=$?
	if [[ ! -e $canary/keep ]]; then
		bad "$name deleted the caller's directory"
	elif ((rc == 0 || rc == 124)); then
		bad "$name did not stop (exit $rc) although mktemp failed"
	else
		ok "$name stops when mktemp fails (exit $rc)"
	fi
done
exit "$fail"
