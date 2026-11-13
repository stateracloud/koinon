#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/docs-examples.sh with a fake binary and throwaway guide files: a passing example passes, a
# broken one fails the run, an empty directory fails, a cluster call fails. Offline; builds nothing.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

unset DOCS_EXAMPLES_DIR KOINON_BIN_DIR DOCS_EXAMPLES_TIMEOUT E1_FILE E1_WORKDIR
here=$(cd "$(dirname "$0")" && pwd)
run=$here/docs-examples.sh
work=$(mktemp -d "${TMPDIR:-/tmp}/docs-examples-test.XXXXXX")
trap 'rm -rf "$work"' EXIT
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }

mkdir "$work/bin" "$work/good" "$work/bad" "$work/empty"
printf '#!/bin/sh\necho "fake 1.2.3"\n' >"$work/bin/koinon-api"
chmod +x "$work/bin/koinon-api"
# shellcheck disable=SC2016 # the $(...) is for the block's shell, not this one
printf '# e1=version\nkoinon-api | grep 1.2.3\n# e1=workdir\ntest -z "$(ls -A)"\n' >"$work/good/a.e1"
printf '# e1=fine\ntrue\n# e1=broken\nkoinon-api | grep nonsense\n' >"$work/bad/b.e1"
printf '# e1=cluster\nkubectl get ns\n' >"$work/bad/c.e1"
printf '# only a header\n' >"$work/empty/d.e1"

# t <description> <want exit> <dir> [args...]
t() {
  local desc=$1 want=$2 d=$3 got=0
  shift 3
  DOCS_EXAMPLES_DIR=$d KOINON_BIN_DIR=$work/bin "$run" --no-build "$@" >"$work/out" 2>&1 || got=$?
  if [[ $got -eq $want ]]; then ok "$desc"; else bad "$desc (want exit $want, got $got)"; sed 's/^/    /' "$work/out" >&2; fi
}

t "passing examples pass" 0 "$work/good"
t "a broken example fails the run" 1 "$work/bad"
if grep -q 'FAIL: b.e1 broken' "$work/out"; then ok "the failing block is named"; else bad "the failing block is not named"; fi
if grep -q 'FAIL: c.e1 cluster' "$work/out"; then ok "a cluster call fails"; else bad "a cluster call did not fail"; fi
if grep -q 'ok: b.e1 fine' "$work/out"; then ok "other blocks still run"; else bad "a block before the failure did not run"; fi
mkdir "$work/after"; printf '# e1=first\nfalse\n# e1=second\ntrue\n' >"$work/after/e.e1"
t "a failing block does not stop the next" 1 "$work/after"
if grep -q 'ok: e.e1 second' "$work/out"; then ok "the block after a failure ran"; else bad "the block after a failure did not run"; fi
mkdir "$work/to"; printf '# e1=slow\nsleep 5\n' >"$work/to/f.e1"
DOCS_EXAMPLES_TIMEOUT=1 t "a block over the timeout fails" 1 "$work/to"
mkdir "$work/env"
# shellcheck disable=SC2016 # expanded by the block's shell
printf '# e1=env\ntest -z "${KOINON_DATABASE_URL:-}"\n' >"$work/env/g.e1"
KOINON_DATABASE_URL=x t "KOINON_* is scrubbed from the block" 0 "$work/env"
t "a file without blocks fails" 1 "$work/empty"
t "a missing directory fails" 1 "$work/nope"
t "--list runs nothing" 0 "$work/bad" --list
rm "$work/bin/koinon-api"
t "a missing binary is a usage error" 2 "$work/good"
t "unknown flag" 2 "$work/good" --bogus

mkdir "$work/mal"; printf '# e1=ok\ntrue\n# e1=Bad\ntrue\n' >"$work/mal/h.e1"
t "--list fails on a malformed file" 1 "$work/mal" --list
printf '#!/bin/sh\necho fake\n' >"$work/bin/koinon-api"; chmod +x "$work/bin/koinon-api"
mkdir "$work/rel"; printf '# e1=v\nkoinon-api | grep -q fake\n' >"$work/rel/i.e1"
if (cd "$work" && DOCS_EXAMPLES_DIR=$work/rel KOINON_BIN_DIR=bin "$run" --no-build >"$work/out" 2>&1); then ok "a relative KOINON_BIN_DIR works"; else bad "a relative KOINON_BIN_DIR fails"; fi

# The checked-in guide files are valid and non-empty.
if DOCS_EXAMPLES_DIR=$here/../test/docs-examples "$run" --list >"$work/out" 2>&1 && [[ -s $work/out ]]; then
  ok "the checked-in guide files list their blocks"
else
  bad "the checked-in guide files do not list"
fi
exit $fail
