#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Document metadata lint (ADR 0115, docs/dev/doc-metadata.md). Every ADR (docs/adr/NNNN-*.md, not
# the 0000 template), research note (docs/research/*.md) and spike (docs/spikes/*.md) carries a
# header: the `- Key: value` lines between its `# Title` and its first `## ` heading.
#
#   ADRs              - Status: and - Date: (checked by hack/adr-lint.sh), - Area:, - Tags:
#   research, spikes  - Status: Draft | Complete | Abandoned | Superseded by <link>, - Date:, - Area:, - Tags:
#
# Area is one area of docs/metadata/areas.txt. Tags are one to six tags of docs/metadata/tags.txt,
# comma-separated, no repeats. Date is YYYY-MM-DD. The template names the Area and Tags fields.
#
# Usage: hack/doc-meta-lint.sh [docs-dir]    (default docs)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
docs=${1:-$here/../docs}
fail=0
err() { echo "doc-meta-lint: $*" >&2; fail=1; }

vocab() { grep -vE '^[[:space:]]*(#|$)' "$1" | awk '{print $1}'; }
[[ -f $docs/metadata/areas.txt && -f $docs/metadata/tags.txt ]] || { err "docs/metadata/areas.txt or tags.txt is missing"; exit 1; }
declare -A areas=() tags=()
while IFS= read -r a; do areas[$a]=1; done < <(vocab "$docs/metadata/areas.txt")
while IFS= read -r t; do
  [[ $t =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]] || err "tags.txt: '$t' is not lower-case kebab-case"
  [[ -z ${tags[$t]:-} ]] || err "tags.txt: '$t' is listed twice"
  tags[$t]=1
done < <(vocab "$docs/metadata/tags.txt")

# meta <file>: one awk pass over the lines before the first `## ` heading (a process per field made the
# lint slow, #990). Sets META_COUNT[key] (how many `- Key:` lines) and META_FIRST[key] (the first one's
# value) for Area, Tags, Status and Date; the template check uses META_COUNT alone.
declare -A META_COUNT META_FIRST
meta() {
  META_COUNT=() META_FIRST=()
  local k c v
  while IFS=$'\t' read -r k c v; do META_COUNT[$k]=$c META_FIRST[$k]=$v; done < <(awk '
    /^## / { exit }
    /^- (Area|Tags|Status|Date):/ {
      k = $0; sub(/^- /, "", k); sub(/:.*/, "", k)
      v = $0; sub(/^- [A-Za-z]+: */, "", v)
      if (!(k in n)) first[k] = v
      n[k]++
    }
    END { for (k in n) printf "%s\t%d\t%s\n", k, n[k], first[k] }' "$1")
}

check_common() { # check_common <file> <name>
  local f=$1 name=$2 area tagline n t
  declare -A seen=()
  meta "$f"
  n=${META_COUNT[Area]:-0}
  if ((n != 1)); then err "$name: needs exactly one '- Area:' line above the first ## heading (found $n)"; else
    area=${META_FIRST[Area]}
    [[ -n ${areas[$area]:-} ]] || err "$name: Area '$area' is not in docs/metadata/areas.txt"
  fi
  n=${META_COUNT[Tags]:-0}
  if ((n != 1)); then err "$name: needs exactly one '- Tags:' line above the first ## heading (found $n)"; return; fi
  tagline=${META_FIRST[Tags]}
  local count=0
  IFS=',' read -ra list <<<"$tagline"
  for t in "${list[@]}"; do
    t=${t//[[:space:]]/}
    [[ -n $t ]] || { err "$name: empty tag in '$tagline'"; continue; }
    [[ -n ${tags[$t]:-} ]] || err "$name: tag '$t' is not in docs/metadata/tags.txt"
    [[ -z ${seen[$t]:-} ]] || err "$name: tag '$t' is listed twice"
    seen[$t]=1
    count=$((count + 1))
  done
  ((count >= 1 && count <= 6)) || err "$name: needs one to six tags (found $count)"
}

checked=0
for f in "$docs"/adr/[0-9][0-9][0-9][0-9]-*.md; do
  [[ -e $f ]] || continue
  name=adr/$(basename "$f")
  if [[ $(basename "$f") == 0000-* ]]; then
    meta "$f"
    [[ -n ${META_COUNT[Area]:-} ]] || err "$name: the template must name the '- Area:' field"
    [[ -n ${META_COUNT[Tags]:-} ]] || err "$name: the template must name the '- Tags:' field"
    continue
  fi
  check_common "$f" "$name"
  checked=$((checked + 1))
done

for f in "$docs"/research/*.md "$docs"/spikes/*.md; do
  [[ -e $f ]] || continue
  [[ $(basename "$f") != README.md ]] || continue
  name=${f#"$docs"/}
  meta "$f"
  status=${META_FIRST[Status]:-}
  grep -qE '^(Draft|Complete|Abandoned|Superseded by)([^A-Za-z]|$)' <<<"$status" ||
    err "$name: '- Status:' must start with Draft, Complete, Abandoned or Superseded by (got '${status:-nothing}')"
  date=${META_FIRST[Date]:-}
  [[ $date =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}([^0-9]|$) ]] || err "$name: '- Date:' must be YYYY-MM-DD (got '${date:-nothing}')"
  check_common "$f" "$name"
  checked=$((checked + 1))
done

if ((fail == 0)); then echo "doc-meta-lint: $checked documents, areas and tags valid"; fi
exit "$fail"
