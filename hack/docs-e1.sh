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
# starts a block; the lines up to the next marker are its commands. The docs site (stateracloud/koinon-docs)
# shows the same commands in fenced blocks whose info string is `sh e1=<name>`; its CI compares each page
# with the file at the release in its koinon.ref. This script reads only the command files.
#
#   docs-e1.sh names          the block names, in file order
#   docs-e1.sh show <name>    one block's commands
#   docs-e1.sh script         every block, each under a "# e1=<name>" line
#   docs-e1.sh run <name>     run one block with bash -euo pipefail in $E1_WORKDIR (default: .);
#                             a one-command block replaces the shell (bash -c), so it can be killed
#
# Which file it reads: $E1_FILE (default test/e2e/commands/quickstart.e1).
#
# Limits of the format:
#   - Empty lines (no character at all) are not part of a block. Any other line is kept byte for byte,
#     also a line that holds only spaces or only a tab. An empty line inside a heredoc body is therefore
#     dropped: keep a heredoc body free of empty lines (or put a space on the line).
#   - A line that is exactly `# e1=<name>` always starts a new block, also inside a heredoc body. Do not
#     write such a line in a heredoc.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
FILE=${E1_FILE:-$root/test/e2e/commands/quickstart.e1}

# blocks prints "<name>\t<line>" for each command line of each e1 block, in order, and "<name>\t" at
# the start of each block.
blocks() {
  awk '
    /^#[ ]?e1=/ && $0 !~ /^# e1=[a-z0-9-]+$/ {
      print "docs-e1: " FILENAME ":" FNR ": invalid e1 marker (want # e1=<lowercase-name>): " $0 > "/dev/stderr"; exit 2
    }
    match($0, /^# e1=[a-z0-9-]+$/) {
      name = substr($0, 6)
      if (name in seen) { print "duplicate e1 block: " name > "/dev/stderr"; exit 2 }
      seen[name] = 1; print name "\t"; next
    }
    name != "" { print name "\t" $0; next }
    $0 !~ /^[[:space:]]*(#.*)?$/ {
      print "docs-e1: " FILENAME ":" FNR ": command line before the first # e1=<name> marker" > "/dev/stderr"; exit 2
    }
  ' "$FILE"
}

names() { blocks | cut -f1 | uniq; }

show() {
  local n=$1 out
  # Split at the first tab only: a command line may start with a tab (a <<-EOF body), and a field
  # split would read it as an empty second field and drop it.
  out=$(blocks | awk -v n="$n" '{ p = index($0, "\t"); if (substr($0, 1, p - 1) == n && length($0) > p) print substr($0, p + 1) }')
  [[ -n $out ]] || { echo "docs-e1: no e1 block named $n in $FILE" >&2; exit 1; }
  printf '%s\n' "$out"
}

case ${1:-} in
names) names ;;
show) show "${2:?name}" ;;
script)
  list=$(names) || exit
  for n in $list; do
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
