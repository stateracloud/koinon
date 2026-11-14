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

# Status and links (old issue 525), in ONE awk pass over README.md and every ADR (#990: a process per file
# made the lint slow, the fork cost dominates on a Mac). The template (0000) lists the values; it is not
# checked. awk prints `E<TAB>message` for a failure and `P<TAB>NNNN` for an ADR still Proposed.
proposed=()
while IFS=$'\t' read -r kind msg; do
  case $kind in
    E) err "$msg" ;;
    P) proposed+=("$msg") ;;
  esac
done < <(cd "$dir" && ADR_FILES="$(printf '%s\n' "${files[@]}")" awk '
  BEGIN { nf = split(ENVIRON["ADR_FILES"], names, "\n"); for (i = 1; i <= nf; i++) have[names[i]] = 1 }
  function bad(m) { printf "E\t%s\n", m }
  # links(text): every [NNNN](target) of one Status or Relates to item must name an ADR file that exists.
  function links(f, text,    t, link, num, target) {
    t = text
    while (match(t, /\[[0-9][0-9][0-9][0-9]\]\([^)]*\)/)) {
      link = substr(t, RSTART, RLENGTH); t = substr(t, RSTART + RLENGTH)
      num = substr(link, 2, 4); target = substr(link, 8, length(link) - 8)
      sub(/#.*/, "", target)
      if (!(target in have)) bad(f ": link " link ": " target " does not exist")
      else if (substr(target, 1, 4) != num) bad(f ": link " link ": the text says " num ", the file is " substr(target, 1, 4))
    }
  }
  function finish(   st, row) {
    if (cur == "" || substr(cur, 1, 4) == "0000") return
    st = status
    if (st !~ /^(Proposed|Accepted|Deprecated|Superseded by|Rejected)([^A-Za-z]|$)/)
      bad(cur ": Status must start with Proposed, Accepted, Deprecated, Superseded by or Rejected (got \x27" (st == "" ? "nothing" : st) "\x27)")
    if (st ~ /^Proposed/) printf "P\t%s\n", substr(cur, 1, 4)
    links(cur, items)
    if (st ~ /^Superseded by/) {
      if (st !~ /\[[0-9][0-9][0-9][0-9]\]\([^)]+\)/) bad(cur ": Superseded by names no ADR link ([NNNN](NNNN-title.md))")
      row = readme["| [" substr(cur, 1, 4) "](" cur ") |"]
      if (row !~ / \| Superseded by/) bad(cur " is superseded, but the README index does not say so; run hack/adr-index.sh")
    }
  }
  FILENAME == "README.md" {
    # the first README line holding "| [NNNN](file) |" is the ADR row
    if (match($0, /\| \[[0-9][0-9][0-9][0-9]\]\([^)]*\) \|/)) { k = substr($0, RSTART, RLENGTH); if (!(k in readme)) readme[k] = $0 }
    next
  }
  FNR == 1 { finish(); cur = FILENAME; status = ""; items = ""; on = 0; seen = 0 }
  /^- Status:/ { if (!seen) { seen = 1; status = $0; sub(/^- Status: */, "", status) } }
  /^- (Status|Relates to):/ { on = 1; items = items "\n" $0; next }
  on && /^  / { items = items "\n" $0; next }
  { on = 0 }
  END { finish() }' README.md "${files[@]}")
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

declare -A exists=()
for f in "${files[@]}"; do exists[${f:0:4}]=1; done
# need_exist <ADR> <numbers>: each number named on an Amends / Supersedes / Reduces / Amended by /
# Superseded by / Reduced by line must be an ADR file (the Status and Relates to links are checked above).
need_exist() {
  local t
  for t in $2; do
    [[ -n ${exists[$t]:-} ]] || err "ADR $1 names ADR $t on its $key line, but no ADR file $t exists"
  done
}
declare -A outs=() ptr_strict=() ptr_any=()
while IFS=$'\t' read -r n item; do
  [[ -n $n && $n != 0000 ]] || continue
  key=${item#- } key=${key%%:*} text=${item#*: }
  case $key in
    Amends | Supersedes | "Supersedes in part" | Reduces)
      if [[ $text != none* ]]; then adr_nums "$text"; outs[$n]+="$REPLY"; need_exist "$n" "$REPLY"; fi ;;
    "Amended by" | "Superseded by" | "Reduced by")
      adr_nums "$text"; ptr_strict[$n]+="$REPLY"; need_exist "$n" "$REPLY"
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
for s in "${!outs[@]}"; do
  for t in ${outs[$s]}; do
    [[ -n ${exists[$t]:-} ]] || continue # a missing target is reported when the header line is read
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
