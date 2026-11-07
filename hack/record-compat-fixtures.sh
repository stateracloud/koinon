#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Record the persisted-state compatibility folder of a release (old issue 652): the stored agent configs,
# the home_checkpoints rows with their schema version and a kopia home backup, all written by the
# code of the checkout you run this in. internal/compat loads every folder under testdata/compat/
# with the code at head.
#
# Usage: hack/record-compat-fixtures.sh <release> [--out DIR]
#   <release>  the tag the folder stands for, e.g. v0.1.0 (the release checklist in
#              docs/dev/releasing.md runs this from the tagged commit)
#   --out DIR  where to write (default testdata/compat/<release>); must be empty or absent
# A recorded folder is never overwritten and never edited by hand (docs/dev/ci.md).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."

release=
out=
while (($#)); do
	case $1 in
	--out) out=${2:?--out needs a directory}; shift 2 ;;
	-h | --help | help) sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	-*) echo "unknown flag: $1" >&2; exit 2 ;;
	*) [[ -z $release ]] || { echo "one release only" >&2; exit 2; }; release=$1; shift ;;
	esac
done
[[ -n $release ]] || { echo "usage: hack/record-compat-fixtures.sh <release> [--out DIR]" >&2; exit 2; }
[[ $release =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || { echo "release name $release: letters, digits, . _ - only" >&2; exit 2; }
out=${out:-testdata/compat/$release}

exec go run ./internal/compat/cmd/record -release "$release" -out "$out"
