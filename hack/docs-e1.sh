#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The shell commands of the quickstart and of the backup and restore guide are the e2e tests' commands
# (plan item DOC1, ADR 0108). This repository owns them, in command files under test/e2e/commands/:
#
#   quickstart.e1       run by test/e2e/run.sh
#   backup-restore.e1   run by test/e2e/backup-restore.sh
#
# A command file is plain text. Comment lines before the first marker are a header. A line `# e1=<name>`
# starts a block; the lines up to the next marker are its commands. The docs site (today docs/site,
# later stateracloud/koinon-docs) shows the same commands in fenced blocks whose info string is
# `sh e1=<name>`; hack/docs-check.sh (here) and the koinon-docs CI compare the page with the file.
#
#   docs-e1.sh names          the block names, in file order
#   docs-e1.sh show <name>    one block's commands
#   docs-e1.sh script         every block, each under a "# e1=<name>" line (the form both checks compare)
#   docs-e1.sh run <name>     run one block with bash -euo pipefail in $E1_WORKDIR (default: .);
#                             a one-command block replaces the shell (bash -c), so it can be killed
#
# Which text it reads: $E1_FILE (default test/e2e/commands/quickstart.e1), or, when $E1_DOC is set, the
# `sh e1=<name>` blocks of that docs page (Markdown); `run` ignores $E1_DOC. Blank lines are not part of a block.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
FILE=${E1_FILE:-$root/test/e2e/commands/quickstart.e1}
DOC=${E1_DOC:-}
# `run` is what the e2e tests call: it always reads the command file, so an exported E1_DOC in the
# caller's shell cannot make a test run a docs page.
[[ ${1:-} != run ]] || DOC=
SRC=${DOC:-$FILE}

# blocks prints "<name>\t<line>" for each command line of each e1 block, in order, and "<name>\t" at
# the start of each block.
blocks() {
  if [[ -n $DOC ]]; then
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
  else
    awk '
      match($0, /^# e1=[a-z0-9-]+$/) {
        name = substr($0, 6)
        if (name in seen) { print "duplicate e1 block: " name > "/dev/stderr"; exit 2 }
        seen[name] = 1; print name "\t"; next
      }
      name != "" { print name "\t" $0 }
    ' "$FILE"
  fi
}

names() { blocks | cut -f1 | uniq; }

show() {
  local n=$1 out
  out=$(blocks | awk -F'\t' -v n="$n" '$1 == n && NF > 1 && $2 != "" { sub(/^[^\t]*\t/, ""); print }')
  [[ -n $out ]] || { echo "docs-e1: no e1 block named $n in $SRC" >&2; exit 1; }
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
