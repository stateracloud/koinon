#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# ADR lint: the numbers in docs/adr/NNNN-*.md are unique (gaps are allowed: a number can be reserved
# with `hack/claim.sh adr` before its pull request lands), 0000 is the template, and the README index
# is current (hack/adr-index.sh --check). Two pull requests that take the same number collide here, so
# the second one renumbers.
#
# Usage: hack/adr-lint.sh [adr-dir]    (default docs/adr)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
dir=${1:-$here/../docs/adr}
fail=0
err() { echo "adr-lint: $*" >&2; fail=1; }

files=()
while IFS= read -r f; do files+=("$f"); done < <(cd "$dir" && /bin/ls | grep -E '^[0-9]{4}-.*\.md$' | sort)
((${#files[@]})) || { err "no ADR files in $dir"; exit 1; }
[[ ${files[0]:0:4} == 0000 ]] || err "0000-template.md is missing"

prev=""
for f in "${files[@]}"; do
  if [[ ${f:0:4} == "$prev" ]]; then err "number ${f:0:4} is used by more than one file ($f)"; fi
  prev=${f:0:4}
done

"$here/adr-index.sh" --check "$dir" >/dev/null || { "$here/adr-index.sh" --check "$dir" || true; fail=1; }

if ((fail == 0)); then echo "adr-lint: ${#files[@]} files, numbers unique, index current"; fi
exit "$fail"
