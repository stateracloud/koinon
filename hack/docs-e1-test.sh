#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/docs-e1.sh against throwaway command files and docs pages, and that the checked-in command
# files (test/e2e/commands/*.e1) parse and run through it (ADR 0108). Offline; no cluster.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

# An E1_* variable from the caller's shell must not change what these tests read.
unset E1_DOC E1_FILE E1_WORKDIR
here=$(cd "$(dirname "$0")" && pwd)
e1=$here/docs-e1.sh
work=$(mktemp -d "${TMPDIR:-/tmp}/docs-e1-test.XXXXXX")
trap 'rm -rf "$work"' EXIT
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }
# is <description> <want> <got>
is() { if [[ $2 == "$3" ]]; then ok "$1"; else bad "$1 (want '$2', got '$3')"; fi; }

cat >"$work/cmds.e1" <<'X'
# SPDX-License-Identifier: Apache-2.0
#
# header, mentions '# e1=<name>' in a sentence
# e1=first
echo one

echo two
# e1=second
# a comment that belongs to the block
echo three
X

is "names, in file order" "first second" "$(E1_FILE=$work/cmds.e1 "$e1" names | tr '\n' ' ' | sed 's/ $//')"
is "show drops blank lines" $'echo one\necho two' "$(E1_FILE=$work/cmds.e1 "$e1" show first)"
is "a comment line inside a block stays" $'# a comment that belongs to the block\necho three' "$(E1_FILE=$work/cmds.e1 "$e1" show second)"
mkdir "$work/w"
printf '# e1=pwd\npwd -P\n' >"$work/pwd.e1"
is "run in E1_WORKDIR" "$(cd "$work/w" && pwd -P)" "$(E1_FILE=$work/pwd.e1 E1_WORKDIR=$work/w "$e1" run pwd)"
printf '# e1=fails\nfalse\necho not-reached\n' >"$work/fails.e1"
if E1_FILE=$work/fails.e1 "$e1" run fails >/dev/null 2>&1; then bad "run stops at the first failing command"; else ok "run stops at the first failing command"; fi
if E1_FILE=$work/cmds.e1 "$e1" show nope >/dev/null 2>&1; then bad "an unknown block is an error"; else ok "an unknown block is an error"; fi
printf '# e1=a\necho 1\n# e1=a\necho 2\n' >"$work/dup.e1"
if E1_FILE=$work/dup.e1 "$e1" names >/dev/null 2>&1; then bad "a duplicate block name is an error"; else ok "a duplicate block name is an error"; fi

# A command line that starts with a tab (a <<-EOF body) is kept byte for byte, in a file and in a page.
printf '# e1=tabs\ncat <<-EOF\n\tindented\n\t\ttwice\nEOF\n' >"$work/tabs.e1"
want=$'cat <<-EOF\n\tindented\n\t\ttwice\nEOF'
is "a tab-leading line survives show" "$want" "$(E1_FILE=$work/tabs.e1 "$e1" show tabs)"
is "a tab-leading line survives script" $'# e1=tabs\n'"$want" "$(E1_FILE=$work/tabs.e1 "$e1" script)"
printf '%s\n' '```sh e1=tabs' 'cat <<-EOF' $'\tindented' $'\t\ttwice' EOF '```' >"$work/tabs.md"
is "a tab-leading line survives show in a page" "$want" "$(E1_DOC=$work/tabs.md "$e1" show tabs)"
# Command content above the first marker is an error that names the file and the line.
printf '# header\n\necho stray\n# e1=a\necho 1\n' >"$work/stray.e1"
if E1_FILE=$work/stray.e1 "$e1" names >/dev/null 2>"$work/stray.err"; then bad "a command before the first marker is an error"; elif grep -q 'stray.e1:3:' "$work/stray.err"; then ok "a command before the first marker is an error"; else bad "the error names the file and line"; fi

# The same blocks in a docs page give the same script as the command file.
cat >"$work/page.md" <<'X'
Some text.

```sh e1=first
echo one

echo two
```

```sh
echo not-a-block
```

```bash e1=second
# a comment that belongs to the block
echo three
```
X
E1_FILE=$work/cmds.e1 "$e1" script >"$work/file.script"
E1_DOC=$work/page.md "$e1" script >"$work/page.script"
if diff -u "$work/file.script" "$work/page.script" >/dev/null; then ok "a page and a command file with the same blocks give the same script"; else bad "page and file scripts differ"; fi
sed -i.bak 's/echo three/echo THREE/' "$work/page.md"
E1_DOC=$work/page.md "$e1" script >"$work/page.script"
if (cd "$(dirname "$e1")/.." && bash hack/docs-check.sh --update >"$work/upd.out" 2>&1); then bad "docs-check --update is an error"; elif grep -q 'there is nothing to generate' "$work/upd.out"; then ok "docs-check --update is an error"; else bad "docs-check --update says why"; fi
if diff -u "$work/file.script" "$work/page.script" >/dev/null; then bad "a changed command in the page is seen"; else ok "a changed command in the page is seen"; fi
if E1_DOC=$work/page.md E1_FILE=$work/cmds.e1 E1_WORKDIR=$work "$e1" run second >"$work/run.out" 2>&1 && grep -q '^three$' "$work/run.out"; then ok "run reads the command file even when E1_DOC is set"; else bad "run reads the command file even when E1_DOC is set"; fi

# The checked-in command files: names, and every block is valid shell.
for f in "$here"/../test/e2e/commands/*.e1; do
  n=$(E1_FILE=$f "$e1" names | wc -l | tr -d ' ')
  [[ $n -gt 0 ]] || bad "$f has no e1 blocks"
  grep -q '^# SPDX-License-Identifier: Apache-2.0$' "$f" || bad "$f has no SPDX header"
  for b in $(E1_FILE=$f "$e1" names); do
    E1_FILE=$f "$e1" show "$b" >"$work/b.sh"
    bash -n "$work/b.sh" || bad "block $b of $f is not valid shell"
  done
  ok "$(basename "$f"): $n blocks parse"
done

exit "$fail"
