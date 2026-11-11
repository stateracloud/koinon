#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The body of `make generate`. On any failure restores the generated outputs so a failed buf
# (for example buf.build unreachable after `clean: true` wiped web/src/gen) never leaves the
# worktree dirty (#349 / old #464). Test hook: GENERATE_ROOT points at a fixture tree.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
if [[ -n ${GENERATE_ROOT:-} ]]; then
	echo "generate: warning: test hook GENERATE_ROOT is set; this run is not a real generate" >&2
	root=$GENERATE_ROOT
fi
cd "$root" || exit 2
GO=${GO:-go}

# Paths buf (clean:true), sqlc and oapi-codegen may create, delete or rewrite.
outs=(
	internal/gen
	web/src/gen
	internal/db/store
	internal/audit/internal/auditstore
	internal/adminapi/api.gen.go
)

tmp=$(mktemp -d "${TMPDIR:-/tmp}/koinon-generate.XXXXXX") || exit 2
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/snap"
for p in "${outs[@]}"; do
	if [[ -e $p ]]; then
		mkdir -p "$tmp/snap/$(dirname "$p")"
		cp -a "$p" "$tmp/snap/$p"
	fi
done

restore() {
	local p
	for p in "${outs[@]}"; do
		rm -rf "$p"
		if [[ -e $tmp/snap/$p ]]; then
			mkdir -p "$(dirname "$p")"
			cp -a "$tmp/snap/$p" "$p"
		fi
	done
}

# Chain with && and record rc (not `if ! { ...; }`): under `set -e`, a failing command inside an
# `if` test does not stop a brace group, so a later successful step would hide the failure.
gen_rc=0
buf generate \
	&& buf generate --template buf.gen.openshell.yaml \
	&& buf generate --template buf.gen.agentgateway.yaml \
	&& buf generate --template buf.gen.googleapis.yaml \
	&& "$GO" tool sqlc generate \
	&& "$GO" tool oapi-codegen -config api/admin/v1/oapi-codegen.yaml api/admin/v1/openapi.yaml \
	|| gen_rc=$?
if [[ $gen_rc -ne 0 ]]; then
	echo "generate: failed; restoring generated outputs so the tree stays as it was" >&2
	restore
	exit 1
fi
