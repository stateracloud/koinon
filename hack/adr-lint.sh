#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# ADR lint: the numbers in docs/adr/NNNN-*.md are unique and contiguous (0000 is the template), and
# the index in docs/adr/README.md has exactly one row per ADR whose link text and target match the
# file. Two pull requests that each add "the next" ADR collide here, so the second one renumbers.
#
# Usage: hack/adr-lint.sh [adr-dir]    (default docs/adr)
set -euo pipefail

dir=${1:-$(cd "$(dirname "$0")/.." && pwd)/docs/adr}
fail=0
err() { echo "adr-lint: $*" >&2; fail=1; }

files=()
while IFS= read -r f; do files+=("$f"); done < <(cd "$dir" && /bin/ls | grep -E '^[0-9]{4}-.*\.md$' | sort)
((${#files[@]})) || { err "no ADR files in $dir"; exit 1; }

prev=-1
nums=()
for f in "${files[@]}"; do
  n=$((10#${f:0:4}))
  if ((n == prev)); then err "number ${f:0:4} is used by more than one file ($f)"
  elif ((prev >= 0 && n != prev + 1)); then err "numbers jump from $(printf '%04d' "$prev") to ${f:0:4}"; fi
  prev=$n
  nums+=("${f:0:4}")
done
[[ ${files[0]:0:4} == 0000 ]] || err "0000-template.md is missing"

readme=$dir/README.md
[[ -r $readme ]] || { err "$readme is missing"; exit 1; }
rows=$(grep -E '^\| \[[0-9]{4}\]\(' "$readme" || true)
for f in "${files[@]}"; do
  [[ ${f:0:4} != 0000 ]] || continue
  count=$(grep -cF "| [${f:0:4}]($f) |" <<<"$rows" || true)
  case $count in
    1) ;;
    0) err "README index has no row for $f" ;;
    *) err "README index has $count rows for $f" ;;
  esac
done
while IFS= read -r row; do
  [[ -n $row ]] || continue
  text=${row#*\[}; text=${text%%\]*}
  target=${row#*\(}; target=${target%%\)*}
  [[ -f $dir/$target ]] || err "README row [$text] links to missing file $target"
  [[ ${target:0:4} == "$text" ]] || err "README row [$text] links to $target"
done <<<"$rows"

if ((fail == 0)); then echo "adr-lint: ${#files[@]} files, index in sync"; fi
exit "$fail"
