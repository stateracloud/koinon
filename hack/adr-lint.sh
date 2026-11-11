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
# An ADR still Proposed is a warning, not a failure (old issue 525). The status values are the ones in
# ADR 0046 decision 4.
#
# Links go both ways (issue 603): an ADR that lists `- Amends:`, `- Supersedes:` (or "Supersedes in
# part") or `- Reduces:` for another is named in that other ADR's header (its Status, `- Amended by:`,
# `- Superseded by:` or `- Reduced by:`), and an ADR that names another as its amender is listed in that
# one's `- Amends:` line. A number in a parenthetical is a note, not a link; `Amends: none` names nothing.
# That a changed decision carries a marker (ADR 0046) is a review item; the lint cannot see it.
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

# Links both ways (issue 603, ADR 0046 decision 4). A later ADR that lists "Amends", "Supersedes" or
# "Reduces" for an older one must be named in the older one's header (Status, "Amended by", "Superseded
# by" or "Reduced by"), and the other way round. The header is the text before the first "## ".

# header_items <file>...: "NNNN<TAB>- Key: text" for every item of every file's header, continuation
# lines joined to their item. One awk process for all files (a process per file made the lint slow).
header_items() {
  (cd "$dir" && awk '
    function flush() { if (item != "") print num "\t" item; item = "" }
    FNR == 1 { flush(); finished = 0; num = substr(FILENAME, 1, 4) }
    finished { next }
    /^## / { flush(); finished = 1; next }
    /^- [A-Za-z][A-Za-z ]*:/ { flush(); item = $0; next }
    /^  / && item != "" { sub(/^ +/, " "); item = item $0; next }
    { flush() }
    END { flush() }' "$@")
}
# The next two functions set REPLY to the numbers they find, separated by spaces (no subprocess).
# adr_nums <text>: the ADR numbers named as `[NNNN](file)` or `ADR NNNN` outside parentheses.
# A number inside a parenthetical is a note ("implements ADR 0002"), not a link.
adr_nums() {
  local t=$1
  REPLY=""
  while [[ $t =~ \[([0-9]{4})\]\([^\)]*\) ]]; do t=${t/"${BASH_REMATCH[0]}"/@${BASH_REMATCH[1]}@}; done
  while [[ $t =~ ADR\ ([0-9]{4}) ]]; do t=${t/"${BASH_REMATCH[0]}"/@${BASH_REMATCH[1]}@}; done
  while [[ $t =~ \([^\(\)]*\) ]]; do t=${t/"${BASH_REMATCH[0]}"/}; done
  while [[ $t =~ @([0-9]{4})@ ]]; do REPLY+="${BASH_REMATCH[1]} "; t=${t/"${BASH_REMATCH[0]}"/}; done
}
# all_nums <text>: every `[NNNN](file)` in the text, parentheses included.
all_nums() {
  local t=$1
  REPLY=""
  while [[ $t =~ \[([0-9]{4})\]\( ]]; do REPLY+="${BASH_REMATCH[1]} "; t=${t/"${BASH_REMATCH[0]}"/}; done
}

declare -A outs=() ptr_strict=() ptr_any=()
while IFS=$'\t' read -r n item; do
  [[ -n $n && $n != 0000 ]] || continue
  key=${item#- } key=${key%%:*} text=${item#*: }
  case $key in
    Amends | Supersedes | "Supersedes in part" | Reduces)
      if [[ $text != none* ]]; then adr_nums "$text"; outs[$n]+="$REPLY"; fi ;;
    "Amended by" | "Superseded by" | "Reduced by")
      adr_nums "$text"; ptr_strict[$n]+="$REPLY"
      all_nums "$text"; ptr_any[$n]+="$REPLY" ;;
    Status)
      all_nums "$text"; ptr_any[$n]+="$REPLY"
      # The clauses of the status that say who changed this ADR.
      IFS=';' read -ra clauses <<<"$text"
      for clause in "${clauses[@]}"; do
        if [[ $clause == *[Aa]mended* || $clause == *[Ss]uperseded* || $clause == *[Rr]educed* ]]; then
          all_nums "$clause"; ptr_strict[$n]+="$REPLY"
        fi
      done ;;
  esac
done < <(header_items "${files[@]}")
has_num() { [[ " $1" == *" $2 "* ]]; } # has_num "<numbers>" <number>
declare -A exists=()
for f in "${files[@]}"; do exists[${f:0:4}]=1; done
for s in "${!outs[@]}"; do
  for t in ${outs[$s]}; do
    [[ -n ${exists[$t]:-} ]] || continue # a missing target is reported by the link check
    has_num "${ptr_any[$t]:-}" "$s" ||
      err "ADR $s amends, supersedes or reduces ADR $t, but ADR $t does not name $s in its Status or Amended by / Superseded by / Reduced by line"
  done
done
for t in "${!ptr_strict[@]}"; do
  for s in ${ptr_strict[$t]}; do
    [[ -n ${exists[$s]:-} ]] || continue
    has_num "${outs[$s]:-}" "$t" ||
      err "ADR $t says it is amended, superseded or reduced by ADR $s, but ADR $s has no Amends / Supersedes / Reduces line naming $t"
  done
done

if ((fail == 0)); then echo "adr-lint: ${#files[@]} files, numbers unique, index current, status and links valid"; fi
exit "$fail"
