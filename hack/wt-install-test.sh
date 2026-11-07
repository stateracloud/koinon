#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests for hack/wt.sh `new`: the web dependency install (old issue 391). A fake pnpm on PATH records
# how it was called; nothing touches the network or a cluster.
# shellcheck disable=SC2015,SC2329 # "A && ok || bad" is the test idiom; cleanup runs from a trap

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
wt=$here/wt.sh
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }
eq() { if [[ $2 == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/wt-install-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
export KUBECONFIG=/dev/null GIT_CONFIG_GLOBAL=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid

git init -q --bare "$tmp/origin.git"
git init -q "$tmp/repo"
mkdir -p "$tmp/repo/web"
echo 'lockfileVersion: 1' >"$tmp/repo/web/pnpm-lock.yaml"
git -C "$tmp/repo" add -A
git -C "$tmp/repo" commit -q -m init
git -C "$tmp/repo" branch -M main
git -C "$tmp/repo" remote add origin "$tmp/origin.git"
git -C "$tmp/repo" push -q origin main

mkdir -p "$tmp/bin"
cat >"$tmp/bin/pnpm" <<'EOS'
#!/usr/bin/env bash
echo "$PWD|$*" >>"$FAKE_PNPM_LOG"
case " $* " in *" --offline "*) [ -z "${FAKE_PNPM_OFFLINE_FAIL:-}" ] || exit 1 ;; esac
[ -z "${FAKE_PNPM_FAIL:-}" ] || exit 1
mkdir -p node_modules
EOS
chmod +x "$tmp/bin/pnpm"
export FAKE_PNPM_LOG=$tmp/pnpm.log KOINON_WT_ROOT=$tmp/kw
: >"$FAKE_PNPM_LOG"

run_new() { (cd "$tmp/repo" && PATH="$tmp/bin:$PATH" "$wt" new "$@" 2>"$tmp/err"); }

out=$(run_new a)
eq "new prints only the path" "$out" "$tmp/kw/a"
[[ -d $tmp/kw/a/web/node_modules ]] && ok "node_modules installed" || bad "node_modules missing"
eq "offline frozen install in web" "$(cat "$FAKE_PNPM_LOG")" "$tmp/kw/a/web|install --frozen-lockfile --offline"

: >"$FAKE_PNPM_LOG"
out=$(FAKE_PNPM_OFFLINE_FAIL=1 run_new b)
eq "offline failure falls back online" "$(wc -l <"$FAKE_PNPM_LOG" | tr -d ' ')" "2"
[[ -d $tmp/kw/b/web/node_modules ]] && ok "fallback installed" || bad "fallback node_modules missing"
grep -q 'retrying online' "$tmp/err" && ok "fallback is announced" || bad "no fallback message"

out=$(FAKE_PNPM_FAIL=1 run_new c)
eq "failed install still creates the worktree" "$out" "$tmp/kw/c"
grep -q 'pnpm install --frozen-lockfile' "$tmp/err" && ok "failure message names the fix" || bad "no fix message"

: >"$FAKE_PNPM_LOG"
out=$(KOINON_WT_NO_INSTALL=1 run_new d)
eq "KOINON_WT_NO_INSTALL skips pnpm" "$(wc -c <"$FAKE_PNPM_LOG" | tr -d ' ')" "0"

[[ $fail -eq 0 ]] && echo "all ok"
exit "$fail"
