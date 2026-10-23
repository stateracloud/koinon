#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The quickstart's shell commands are the E1 e2e test's commands (plan item DOC1). A fenced block
# whose info string is `sh e1=<name>` in docs/site/src/content/docs/quickstart.md is run by
# test/e2e/run.sh through this script, so the docs and the test cannot disagree.
#
#   docs-e1.sh names          the block names, in document order
#   docs-e1.sh show <name>    one block's commands
#   docs-e1.sh script         every block, each under a "# e1=<name>" line (the golden form)
#   docs-e1.sh run <name>     run one block with bash -euo pipefail in $E1_WORKDIR (default: .);
#                             a one-command block replaces the shell (bash -c), so it can be killed
set -euo pipefail

DOC=${E1_DOC:-$(cd "$(dirname "$0")/.." && pwd)/docs/site/src/content/docs/quickstart.md}

# blocks prints "<name>\t<line>" for each command line of each e1 block, in order.
blocks() {
  awk '
    /^```/ {
      if (name != "") { name = ""; next }
      if (match($0, /^```(sh|bash) +e1=[a-z0-9-]+/)) {
        s = substr($0, RSTART, RLENGTH); sub(/^.*e1=/, "", s); name = s
        if (name in seen) { print "duplicate e1 block: " name > "/dev/stderr"; exit 2 }
        seen[name] = 1; print name "\t"; next
      }
      next
    }
    name != "" { print name "\t" $0 }
  ' "$DOC"
}

names() { blocks | cut -f1 | uniq; }

show() {
  local n=$1 out
  out=$(blocks | awk -F'\t' -v n="$n" '$1 == n && NF > 1 && $2 != "" { sub(/^[^\t]*\t/, ""); print }')
  [[ -n $out ]] || { echo "docs-e1: no e1 block named $n in $DOC" >&2; exit 1; }
  printf '%s\n' "$out"
}

case ${1:-} in
names) names ;;
show) show "${2:?name}" ;;
script)
  for n in $(names); do
    echo "# e1=$n"
    show "$n"
  done
  ;;
run)
  body=$(show "${2:?name}")
  cd "${E1_WORKDIR:-.}"
  exec bash -euo pipefail -c "$body"
  ;;
*)
  echo "usage: docs-e1.sh names | show <name> | script | run <name>" >&2
  exit 2
  ;;
esac
