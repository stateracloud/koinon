#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs golangci-lint with this worktree's own lock and cache, so lanes that lint in several worktrees
# of one machine at once never fail with "parallel golangci-lint is running" (#236).
#
#   hack/golangci-lint.sh <golangci-lint arguments>     e.g. hack/golangci-lint.sh run ./internal/x
#   hack/golangci-lint.sh --print-dirs                  print TMPDIR= and GOLANGCI_LINT_CACHE= and exit
#   hack/golangci-lint.sh --clean                       delete this worktree's lock and cache directory
#                                                       (hack/wt.sh rm and gc call it before removing a worktree)
#
# golangci-lint takes a file lock at $TMPDIR/golangci-lint.lock on start and exits 3 when another
# run holds it. That keeps two runs in one checkout apart, which is right, but a shared TMPDIR (one
# per sandbox, not per worktree) makes it machine-wide. This keys TMPDIR and GOLANGCI_LINT_CACHE by
# a hash of the worktree root (resolved with pwd -P), under ${TMPDIR:-/tmp}/koinon-lint/<hash>/, like
# hack/preflight.sh keys its logs. #236 asks for the cache per worktree too: a lane's first lint
# then runs cold, and --clean removes the directory with the worktree. Two runs in the same worktree still exclude each other. The Go build
# cache (GOCACHE) stays shared: go itself makes it safe for concurrent use.
# KOINON_LINT_DIR=<dir> puts both directories under <dir> instead.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null) || { echo "golangci-lint.sh: not inside a git worktree" >&2; exit 2; }
root=$(cd "$root" && pwd -P)
rootkey=$(printf '%s' "$root" | { shasum -a 256 2>/dev/null || sha256sum; } | cut -c1-12)
dir=${KOINON_LINT_DIR:-${TMPDIR:-/tmp}/koinon-lint/$rootkey}

if [[ ${1:-} == --clean ]]; then
	[[ -n ${KOINON_LINT_DIR:-} ]] || rm -rf "$dir"
	exit 0
fi

mkdir -p "$dir/tmp" "$dir/cache"

if [[ ${1:-} == --print-dirs ]]; then
	echo "TMPDIR=$dir/tmp"
	echo "GOLANGCI_LINT_CACHE=$dir/cache"
	exit 0
fi

TMPDIR=$dir/tmp GOLANGCI_LINT_CACHE=$dir/cache exec golangci-lint "$@"
