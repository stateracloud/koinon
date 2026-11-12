#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints the git ref `buf breaking --against` should use (#542).
#
# Usage:
#   hack/buf-breaking-against.sh <base-ref>              # PR: merge base of HEAD and base-ref
#   hack/buf-breaking-against.sh <base-ref> --merge-group  # merge queue: the base tip itself
#
# A PR that has not merged origin/main must not be compared with main's tip: after main adds a
# .proto field, buf would report that the PR "deleted" it. The merge base is what the PR changed.
# The merge queue already tests the would-be merge commit against the base tip (merge_group.base_sha).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

base=${1:?buf-breaking-against: base ref required}
mode=${2:-}

case $mode in
"" ) git merge-base "$base" HEAD ;;
--merge-group) printf '%s\n' "$base" ;;
*)
	echo "buf-breaking-against: unknown mode $mode (use --merge-group or nothing)" >&2
	exit 2
	;;
esac
