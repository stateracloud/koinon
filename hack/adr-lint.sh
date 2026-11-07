#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# ADR lint: the numbers in docs/adr/NNNN-*.md are unique (gaps are allowed: a number can be reserved
# with `hack/claim.sh adr` before its pull request lands), 0000 is the template, and the README index
# is current (hack/adr-index.sh --check). Two pull requests that take the same number collide here, so
# the second one renumbers.
#
# Each ADR's `- Status:` starts with Proposed, Accepted, Deprecated, Superseded by or Rejected (free
# text may follow); every [NNNN](file.md) link in the Status and `- Relates to:` lines names an ADR
# file that exists; a superseded ADR names its successor and shows as superseded in the README index.
# An ADR still Proposed is a warning, not a failure (old issue 525).
#
# Usage: hack/adr-lint.sh [adr-dir]    (default docs/adr)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

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

# items <file>: the `- Status:` and `- Relates to:` items, with their indented continuation lines.
items() {
  awk '/^- (Status|Relates to):/ { on = 1; print; next } on && /^  / { print; next } { on = 0 }' "$1"
}

# Status and links (old issue 525). The template (0000) lists the values; it is not checked.
proposed=()
for f in "${files[@]}"; do
  n=${f:0:4}
  [[ $n != 0000 ]] || continue
  status=$(sed -n 's/^- Status: *//p' "$dir/$f" | head -1)
  if ! grep -qE '^(Proposed|Accepted|Deprecated|Superseded by|Rejected)([^A-Za-z]|$)' <<<"$status"; then
    err "$f: Status must start with Proposed, Accepted, Deprecated, Superseded by or Rejected (got '${status:-nothing}')"
  fi
  [[ $status != Proposed* ]] || proposed+=("$n")
  # Every [NNNN](file) link in Status and Relates to names an ADR file that exists, with its number.
  while IFS= read -r link; do
    [[ -n $link ]] || continue
    num=${link:1:4} target=${link#*](} target=${target%)} target=${target%%#*}
    if [[ ! -e $dir/$target ]]; then
      err "$f: link $link: $target does not exist"
    elif [[ ${target:0:4} != "$num" ]]; then
      err "$f: link $link: the text says $num, the file is ${target:0:4}"
    fi
  done < <(items "$dir/$f" | grep -oE '\[[0-9]{4}\]\([^)]*\)' || true)
  if [[ $status == "Superseded by"* ]]; then
    grep -qE '\[[0-9]{4}\]\([^)]+\)' <<<"$status" || err "$f: Superseded by names no ADR link ([NNNN](NNNN-title.md))"
    # The README index shows it as superseded.
    row=$(grep -F "| [$n]($f) |" "$dir/README.md" | head -1 || true)
    [[ $row == *" | Superseded by"* ]] ||
      err "$f is superseded, but the README index does not say so; run hack/adr-index.sh"
  fi
done
for n in ${proposed[@]+"${proposed[@]}"}; do
  echo "adr-lint: warning: ADR $n is still Proposed; accept or reject it" >&2
done

if ((fail == 0)); then echo "adr-lint: ${#files[@]} files, numbers unique, index current, status and links valid"; fi
exit "$fail"
