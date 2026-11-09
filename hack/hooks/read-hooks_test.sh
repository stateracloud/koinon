#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/hooks/read-result-check.sh and hack/hooks/bash-read-check.sh (old issue 534).
# Needs only bash, awk and jq.
# shellcheck disable=SC2016 # the commands under test are literal strings; nothing should expand

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }

# decide <hook> <json>: prints allow or deny.
decide() {
  local out
  out=$("$here/$1" <<<"$2")
  if [ -z "$out" ]; then echo allow; else jq -r '.hookSpecificOutput.permissionDecision' <<<"$out"; fi
}

# read_case <want> <file_path> [limit]
read_case() {
  local json got
  if [ $# -ge 3 ]; then
    json=$(jq -nc --arg p "$2" --argjson l "$3" '{tool_name:"Read",tool_input:{file_path:$p,limit:$l}}')
  else
    json=$(jq -nc --arg p "$2" '{tool_name:"Read",tool_input:{file_path:$p}}')
  fi
  got=$(decide read-result-check.sh "$json")
  if [ "$got" = "$1" ]; then ok "Read $2 ${3:-}: $1"; else bad "Read $2 ${3:-}: got $got, want $1"; fi
}

# bash_case <want> <command>
bash_case() {
  local got
  got=$(decide bash-read-check.sh "$(jq -nc --arg c "$2" '{tool_name:"Bash",tool_input:{command:$c}}')")
  if [ "$got" = "$1" ]; then ok "Bash $1: $(head -1 <<<"$2")"; else bad "Bash: got $got, want $1: $2"; fi
}

tr=/root/.claude/projects/p/abc/tool-results/toolu_01.txt
read_case deny "$tr"
read_case allow "$tr" 200
read_case allow /home/user/koinon/internal/api/handler.go
read_case allow /tmp/x/tool-results-summary.md

# Allowed.
bash_case allow 'grep -n Register internal/api/handler.go'
bash_case allow 'cat internal/api/handler.go | grep -n Register'
bash_case allow 'cat a.go b.go | wc -l'
bash_case allow "cat > /tmp/f.go <<'EOF'
cat a.go b.go
sed -n 1p x.go; sed -n 2p y.go
EOF
go vet /tmp/f.go"
bash_case allow 'cat <<EOF
hello
EOF'
bash_case allow 'cat /dev/null'
bash_case allow 'cat /tmp/build.log'
bash_case allow 'cat /tmp/claude-0/tasks/b1x2.output'
bash_case allow 'gh pr create --title x --body "cat a.go b.go; sed -n 1p x.go; sed -n 2p y.go"'
bash_case allow "git commit -s -m 'docs: cat a.md b.md, for f in *; do cat \$f; done'"
bash_case allow 'sed -n 1,40p internal/api/handler.go'
bash_case allow 'head -n 20 internal/api/handler.go'
bash_case allow 'cat VERSION'
bash_case allow 'v=$(cat charts/koinon/values.yaml); echo "${#v}"'
bash_case allow 'for f in a.go b.go; do cat "$f" | wc -l; done'
bash_case allow "sed -i 's/a/b/' a.go b.go"
bash_case allow 'tail -f /tmp/x.log'
bash_case allow 'cat a.go > b.go'
bash_case allow 'sed -n 1p a.go | head -1; git status'
bash_case allow 'echo "for f in *; do cat $f; done"'
bash_case allow 'go test ./internal/x 2>&1 | tail -20'
bash_case allow 'jq . <<<"$(cat a.yaml b.yaml)"'
# Several sed -n line ranges in one command, 210 lines or less in total (the cost is the lines).
bash_case allow 'sed -n 1,20p a.go; sed -n 5,9p b.go'
bash_case allow "sed -n '1,20p' a.go && sed -n '30,40p' a.go"
bash_case allow 'sed -n 1p a.go b.go'
bash_case allow 'sed -n 1,5p a.go
sed -n 1,5p b.go'
bash_case allow 'cd /tmp/wt && sed -n 40,80p hack/a.sh; sed -n 100,140p hack/b.sh; git diff --stat | tail -15'
bash_case allow 'sed -n 1,100p a.go; sed -n 1,100p b.go'
bash_case allow 'sed -n 1,200p a.go'
bash_case allow 'sed -n 60,260p a.go'
bash_case allow 'sed -n 1,150p a.go; sed -n 1,60p b.go'
bash_case allow 'sed -n "10,+5p;40p;41q" a.go b.go'
bash_case allow 'sed -n -e 1,20p -e 40,60p a.go'
bash_case allow 'sed -n -e1,20p --expression=40,60p a.go'
bash_case allow "sed -n '1,20 p' a.go; sed -n 5,9p b.go"
bash_case allow "sed -n '/^func Register/,/^}/p' internal/api/handler.go"
bash_case allow 'sed -n 1,900p a.go | grep -n Register; sed -n 1,20p b.go'

# Refused.
bash_case deny 'cat a.go b.go'
bash_case deny 'cat internal/api/handler.go'
bash_case deny 'cat -n hack/preflight.sh'
# sed -n: more than 210 lines in total, or several reads when the size of one is unknown.
bash_case deny 'sed -n 1,150p a.go; sed -n 1,61p b.go'
bash_case deny 'sed -n 1,211p a.go'
bash_case deny 'sed -n 1,150p a.go b.go'
bash_case deny 'sed -n -e 1,100p -e 200,320p a.go'
bash_case deny 'sed -n -e1,9999p a.go'
bash_case deny 'sed -n --expression=1,9999p a.go'
bash_case deny "sed -n '1,500 p' a.go"
bash_case deny 'sed -n --file=print.sed a.go b.go'
bash_case deny 'sed -n 1,71p a.go
sed -n 1,71p b.go
sed -n 1,71p c.go'
bash_case deny "sed -n '/^func A/,/^}/p' a.go; sed -n 1,5p b.go"
bash_case deny 'sed -n "${a},${b}p" a.go; sed -n 1,5p b.go'
bash_case deny "sed -n '100,\$p' a.go && sed -n 1p b.go"
bash_case deny 'sed -n -f print.sed a.go b.go'
bash_case deny 'for f in a.go b.go; do cat "$f"; done'
bash_case deny 'for f in *.go
do
  sed -n 1,5p "$f"
done'
bash_case deny 'while read -r f; do head -20 "$f"; done < list.txt'
bash_case deny 'head -n 5 a.go b.go'
bash_case deny 'tail -n 3 a.md b.md'
bash_case deny 'cd /home/user/koinon && cat README.md'
bash_case deny 'cat "$dir"/main.go'
bash_case deny '/bin/cat charts/koinon/values.yaml'
bash_case deny 'git status; cat docs/design.md'
bash_case deny 'cat a.txt b.txt'
bash_case deny 'ls; for f in a b; do tail -n 3 $f; done'
bash_case deny 'cat a.go 2>/dev/null'

out=$("$here/bash-read-check.sh" <<<'{"tool_input":{"command":"cat a.go b.go"}}')
reason=$(jq -r '.hookSpecificOutput.permissionDecisionReason' <<<"$out")
if [ "$(jq -r '.hookSpecificOutput.hookEventName' <<<"$out")" = PreToolUse ] && [ "$(printf '%s\n' "$reason" | wc -l)" -eq 1 ] \
  && [[ $reason == *"Read with offset and limit"* ]] && [[ $reason == *"Edit"* ]]; then
  ok "deny output shape (one-line reason naming the alternative and the Edit rule)"
else
  bad "deny output shape ($out)"
fi
out=$("$here/bash-read-check.sh" <<<'{"tool_input":{"command":"sed -n 1,150p a.go; sed -n 1,61p b.go"}}')
if [[ $(jq -r '.hookSpecificOutput.permissionDecisionReason' <<<"$out") == *"211 lines"*"limit is 210"* ]]; then
  ok "the sed -n reason gives the total and the limit"
else
  bad "sed -n reason ($out)"
fi
if [ -z "$("$here/bash-read-check.sh" <<<'{"tool_input":{}}')" ]; then ok "no command: allow"; else bad "no command"; fi

# A command over 64 KiB (a generated script, pasted data) is allowed without parsing, and fast: the
# tokenizer's cost grows with size, and quadratically in some awks.
big=$(jq -nc --arg c "$(printf 'cat a.go b.go\n%.0s' $(seq 6000))" '{tool_input:{command:$c}}')
start=$SECONDS
got=$(decide bash-read-check.sh "$big")
if [ "$got" = allow ] && [ $((SECONDS - start)) -le 2 ]; then ok "a >64 KiB command allows fast"; else bad "a >64 KiB command: got $got in $((SECONDS - start))s"; fi

exit "$fail"
