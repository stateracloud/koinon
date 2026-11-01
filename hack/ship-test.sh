#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/ship.sh: temp git repositories with a bare "origin", and `gh`, `gitleaks` and
# the hack/ helpers (needs-e2e.sh, claim.sh) as stubs. Nothing talks to GitHub.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
ship=$repo/hack/ship.sh
tmp=$(mktemp -d)
trap '[[ -n ${KEEP_TMP:-} ]] || rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
export PATH="$tmp/bin:$PATH" FAKE_LOG="$tmp/calls.log" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_TERMINAL_PROMPT=0 FAKE_NEEDS_RC="$tmp/needs.rc" FAKE_CLAIM_RC="$tmp/claim.rc"
: >"$FAKE_LOG"
echo 1 >"$FAKE_NEEDS_RC"
echo 0 >"$FAKE_CLAIM_RC"

cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "gh $*" >>"$FAKE_LOG"
case "$1 $2" in
"pr list") echo "${FAKE_PR_URL:-}" ;;
"pr create") echo "https://example.invalid/o/r/pull/123" ;;
"project item-add") exit "${FAKE_BOARD_RC:-0}" ;;
esac
exit 0
SH
cat >"$tmp/bin/gitleaks" <<'SH'
#!/usr/bin/env bash
echo "gitleaks $*" >>"$FAKE_LOG"
[[ ${FAKE_GITLEAKS_RC:-0} -eq 0 ]] || echo "Finding: fake leak"
exit "${FAKE_GITLEAKS_RC:-0}"
SH
chmod +x "$tmp/bin/gh" "$tmp/bin/gitleaks"

git init -q --bare -b main "$tmp/origin.git"
seed=$tmp/seed
git init -q -b main "$seed"
git -C "$seed" config user.name seed
git -C "$seed" config user.email seed@example.invalid
mkdir -p "$seed/hack" "$seed/docs/adr"
cp "$repo/hack/adr-index.sh" "$seed/hack/"
cat >"$seed/hack/needs-e2e.sh" <<'SH'
#!/usr/bin/env bash
echo "needs-e2e $*" >>"$FAKE_LOG"
rc=$(cat "$FAKE_NEEDS_RC")
[[ $rc -ne 0 ]] || echo "kind e2e needed; these changed paths matter:"
exit "$rc"
SH
cat >"$seed/hack/claim.sh" <<'SH'
#!/usr/bin/env bash
echo "claim $*" >>"$FAKE_LOG"
exit "$(cat "$FAKE_CLAIM_RC")"
SH
chmod +x "$seed/hack/"*.sh
printf '# ADRs\n\n## Index\n' >"$seed/docs/adr/README.md"
printf '# 0001. First\n\n- Status: accepted\n' >"$seed/docs/adr/0001-first.md"
(cd "$seed" && hack/adr-index.sh >/dev/null && echo base >a.txt && git add -A && git commit -q -m seed && git remote add origin "$tmp/origin.git" && git push -q origin main)

newwork() { # prints the path of a fresh clone on branch feat/x with an identity
	local w
	w=$(mktemp -d "$tmp/w.XXXXXX")
	git clone -q "$tmp/origin.git" "$w" 2>/dev/null
	git -C "$w" config user.name "Test Dev"
	git -C "$w" config user.email dev@example.invalid
	git -C "$w" checkout -q -b feat/x
	echo "$w"
}
# shellcheck disable=SC2329 # called through check
branch_on_origin() { git --git-dir="$tmp/origin.git" rev-parse --verify -q "refs/heads/$1" >/dev/null; }
reset_state() { : >"$FAKE_LOG"; unset FAKE_PR_URL FAKE_GITLEAKS_RC FAKE_BOARD_RC KOINON_SHIP_ALLOWED_EMAILS KOINON_COMMIT_FOOTER || true; echo 1 >"$FAKE_NEEDS_RC"; echo 0 >"$FAKE_CLAIM_RC"; }

fail=0
pass=0
check() { # <description> <command...>
	local d=$1
	shift
	if "$@" >/dev/null 2>&1; then echo "ok: $d"; pass=$((pass + 1)); else echo "FAIL: $d" >&2; fail=$((fail + 1)); fi
}
# shellcheck disable=SC2329 # called through check
has() { grep -q -- "$1" "$2"; }
# shellcheck disable=SC2329
hasnt() { ! grep -q -- "$1" "$2"; }
# shellcheck disable=SC2329
eq() { [[ $1 == "$2" ]]; }
run() { # <outfile> <args...>: runs ship in the current directory; echoes the exit code
	local out=$1 rc=0
	shift
	"$ship" "$@" >"$out" 2>&1 || rc=$?
	echo "$rc"
}

# 1. identity refusal
reset_state
w=$(newwork); cd "$w"
echo one >f.txt
export KOINON_SHIP_ALLOWED_EMAILS="a@example.invalid, b@example.invalid"
rc=$(run "$tmp/o1" -m "feat: x")
check "identity not allowed: exit 1" eq "$rc" 1
check "identity refusal is named" has "KOINON_SHIP_ALLOWED_EMAILS" "$tmp/o1"
check "identity refusal commits nothing" eq "$(git rev-list --count origin/main..HEAD)" 0
check "identity refusal pushes nothing" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/x"

# 2. sign-off, footer, push, any-email notice
reset_state
w=$(newwork); cd "$w"
echo one >f.txt
export KOINON_COMMIT_FOOTER=$'Co-Authored-By: Bot <bot@example.invalid>\nClaude-Session: https://example.invalid/s'
rc=$(run "$tmp/o2" -m "feat: add f")
check "plain ship exits 0" eq "$rc" 0
check "commit line printed" has '^commit ok [0-9a-f]\{7,\}' "$tmp/o2"
check "unset allow list is printed" has 'any address allowed' "$tmp/o2"
git log -1 --format=%B >"$tmp/m2"
check "sign-off present" has '^Signed-off-by: Test Dev <dev@example.invalid>$' "$tmp/m2"
check "footer appended" has '^Claude-Session: https://example.invalid/s$' "$tmp/m2"
check "gitleaks ran over origin/main..HEAD" has 'gitleaks git --redact --no-banner --log-opts=origin/main..HEAD' "$FAKE_LOG"
check "gitleaks line counts commits" has '^gitleaks ok (1 commits)$' "$tmp/o2"
check "pushed line" has '^pushed feat/x$' "$tmp/o2"
check "branch reached origin" branch_on_origin feat/x
check "no PR without --pr-title" hasnt 'gh pr' "$FAKE_LOG"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/x

# 3. allowed identity passes
reset_state
w=$(newwork); cd "$w"
echo one >f.txt
export KOINON_SHIP_ALLOWED_EMAILS="x@example.invalid,dev@example.invalid"
rc=$(run "$tmp/o3" -m "feat: y")
check "allowed identity: exit 0" eq "$rc" 0
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/x

# 4. blob refusal
reset_state
w=$(newwork); cd "$w"
dd if=/dev/zero of=big.bin bs=1048576 count=6 2>/dev/null
rc=$(run "$tmp/o4" -m "feat: big")
check "6 MiB file: exit 1" eq "$rc" 1
check "blob refusal names the path" has 'big.bin' "$tmp/o4"
check "blob refusal does not commit" eq "$(git rev-list --count origin/main..HEAD)" 0
check "blob refusal does not push" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/x"
# committed earlier by hand: still caught
git add -A && git commit -q -m "oops" && rc=$(run "$tmp/o4b")
check "already committed big blob: exit 1" eq "$rc" 1
check "already committed big blob is named" has 'big.bin' "$tmp/o4b"

# 5. refusing main
reset_state
w=$(newwork); cd "$w"
git checkout -q main
echo z >z.txt
rc=$(run "$tmp/o5" -m "feat: z")
check "main: exit 1" eq "$rc" 1
check "main refusal explained" has 'refusing to ship from main' "$tmp/o5"
check "main: nothing committed" eq "$(git rev-list --count origin/main..HEAD)" 0

# 6. merge of origin/main (never rebase), then push
reset_state
w=$(newwork); cd "$w"
adv=$tmp/adv
git clone -q "$tmp/origin.git" "$adv"
git -C "$adv" config user.name adv && git -C "$adv" config user.email adv@example.invalid
echo more >"$adv/b.txt" && git -C "$adv" add -A && git -C "$adv" commit -q -m "main moves" && git -C "$adv" push -q origin main
echo mine >mine.txt
rc=$(run "$tmp/o6" -m "feat: mine")
check "behind main: exit 0" eq "$rc" 0
check "merged line" has '^merged origin/main$' "$tmp/o6"
check "pushed branch contains the new main" git --git-dir="$tmp/origin.git" merge-base --is-ancestor "$(git -C "$adv" rev-parse HEAD)" refs/heads/feat/x
check "merge commit exists" eq "$(git rev-list --merges --count origin/main..HEAD)" 1
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/x
rm -rf "$adv"

# 7. plain conflict stops with exit 4 and names the path
reset_state
w=$(newwork); cd "$w"
git clone -q "$tmp/origin.git" "$adv"
git -C "$adv" config user.name adv && git -C "$adv" config user.email adv@example.invalid
echo main-side >"$adv/a.txt" && git -C "$adv" commit -qam "main edits a" && git -C "$adv" push -q origin main
echo branch-side >a.txt
rc=$(run "$tmp/o7" -m "feat: edit a")
check "conflict: exit 4" eq "$rc" 4
check "conflict names the path" has 'a.txt' "$tmp/o7"
check "conflict pushes nothing" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/x"
rm -rf "$adv"

# 8. ADR README conflict is regenerated
reset_state
w=$(newwork); cd "$w"
git clone -q "$tmp/origin.git" "$adv"
git -C "$adv" config user.name adv && git -C "$adv" config user.email adv@example.invalid
printf '# 0002. Second\n\n- Status: accepted\n' >"$adv/docs/adr/0002-second.md"
(cd "$adv" && hack/adr-index.sh >/dev/null && git add -A && git commit -q -m "adr 2" && git push -q origin main)
printf '# 0003. Third\n\n- Status: accepted\n' >docs/adr/0003-third.md
hack/adr-index.sh >/dev/null
rc=$(run "$tmp/o8" -m "docs: adr 3")
check "ADR README conflict: exit 0" eq "$rc" 0
check "ADR README regenerated" has 'regenerated docs/adr/README.md' "$tmp/o8"
check "regenerated README has both ADRs" bash -c "grep -q 0002-second.md docs/adr/README.md && grep -q 0003-third.md docs/adr/README.md && ! grep -q '<<<<' docs/adr/README.md"
check "ADR README: pushed" branch_on_origin feat/x
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/x
rm -rf "$adv"

# 9. gitleaks failure stops before the push
reset_state
w=$(newwork); cd "$w"
echo s >s.txt
export FAKE_GITLEAKS_RC=1
rc=$(run "$tmp/o9" -m "feat: s")
check "gitleaks failure: exit 1" eq "$rc" 1
check "gitleaks failure is named" has 'gitleaks' "$tmp/o9"
check "gitleaks failure: no push" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/x"

# 10. PR create, then update
reset_state
w=$(newwork); cd "$w"
echo p >p.txt
echo "body" >"$tmp/body.md"
rc=$(run "$tmp/o10" -m "feat: p" --pr-title "feat: p" --pr-body "$tmp/body.md" --draft)
check "PR create: exit 0" eq "$rc" 0
check "gh pr create called with --draft" has 'gh pr create .*--draft' "$FAKE_LOG"
check "PR line printed" has '^PR #123 https://example.invalid/o/r/pull/123$' "$tmp/o10"
check "board add called" has 'gh project item-add 1 --owner stateracloud --url https://example.invalid/o/r/pull/123' "$FAKE_LOG"
check "board ok line" has '^board ok$' "$tmp/o10"
reset_state
export FAKE_PR_URL=https://example.invalid/o/r/pull/77
echo q >q.txt
rc=$(run "$tmp/o10b" -m "feat: q" --pr-title "feat: p" --pr-body "$tmp/body.md")
check "PR update: exit 0" eq "$rc" 0
check "existing PR: gh pr edit with the body file" has "gh pr edit https://example.invalid/o/r/pull/77 .*--body-file $tmp/body.md" "$FAKE_LOG"
check "existing PR: no create" hasnt 'gh pr create' "$FAKE_LOG"
check "existing PR number reported" has '^PR #77 ' "$tmp/o10b"
# board failure is a warning only
reset_state
export FAKE_PR_URL=https://example.invalid/o/r/pull/77 FAKE_BOARD_RC=1
echo r >r.txt
rc=$(run "$tmp/o10c" -m "feat: r" --pr-title "t" --pr-body "$tmp/body.md")
check "board failure is best effort: exit 0" eq "$rc" 0
check "board failure warns" has '^board warn$' "$tmp/o10c"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/x

# 11. ready, auto-merge refused when the e2e is needed, allowed when not
reset_state
w=$(newwork); cd "$w"
echo e >e.txt
echo 0 >"$FAKE_NEEDS_RC"
rc=$(run "$tmp/o11" -m "feat: e" --pr-title "t" --pr-body "$tmp/body.md" --ready --auto-merge)
check "needs e2e: auto-merge refused, exit 1" eq "$rc" 1
check "refusal says the e2e must be green" has 'needs the kind e2e' "$tmp/o11"
check "no gh pr merge when refused" hasnt 'gh pr merge' "$FAKE_LOG"
check "ready still happened before the refusal" has '^ready$' "$tmp/o11"
reset_state
echo 1 >"$FAKE_NEEDS_RC"
export FAKE_PR_URL=https://example.invalid/o/r/pull/123
echo e2 >e2.txt
rc=$(run "$tmp/o11b" -m "feat: e2" --pr-title "t" --pr-body "$tmp/body.md" --auto-merge)
check "e2e not needed: exit 0" eq "$rc" 0
check "auto-merge on line" has '^auto-merge on$' "$tmp/o11b"
check "gh pr merge --auto --merge" has 'gh pr merge .* --auto --merge' "$FAKE_LOG"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/x

# 11c. (#392) --e2e: the label goes on an open PR before the push, on a new PR at creation
reset_state
unset FAKE_PR_URL
w=$(newwork); cd "$w"
echo e3 >e3.txt
rc=$(run "$tmp/o11c" -m "feat: e3" --pr-title "t" --pr-body "$tmp/body.md" --e2e)
check "--e2e new PR: exit 0" eq "$rc" 0
check "--e2e new PR: created with the label" has 'gh pr create .*--label e2e' "$FAKE_LOG"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/x
reset_state
export FAKE_PR_URL=https://example.invalid/o/r/pull/123
echo e4 >e4.txt
rc=$(run "$tmp/o11d" -m "feat: e4" --pr-title "t" --pr-body "$tmp/body.md" --e2e)
check "--e2e open PR: exit 0" eq "$rc" 0
check "--e2e open PR: label added" has 'gh pr edit .* --add-label e2e' "$FAKE_LOG"
check "--e2e open PR: label step before the push" has '^label e2e$' "$tmp/o11d"
unset FAKE_PR_URL
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/x

# 12. claim review, and a lost claim
reset_state
w=$(newwork); cd "$w"
echo c >c.txt
rc=$(run "$tmp/o12" -m "feat: c" --review 7,8)
check "review: exit 0" eq "$rc" 0
check "review lines" has '^review ok #8$' "$tmp/o12"
check "claim.sh review called per issue" has '^claim review 7$' "$FAKE_LOG"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/x
reset_state
echo 3 >"$FAKE_CLAIM_RC"
w=$(newwork); cd "$w"
echo c >c.txt
rc=$(run "$tmp/o12b" -m "feat: c" --review 7)
check "claim lost: exit 3" eq "$rc" 3
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/x

# 13. dry run does nothing
reset_state
w=$(newwork); cd "$w"
echo d >d.txt
rc=$(run "$tmp/o13" -m "feat: d" --pr-title t --pr-body "$tmp/body.md" --ready --auto-merge --review 5 --dry-run)
check "dry run: exit 0" eq "$rc" 0
check "dry run prints steps" has '^dry-run: would' "$tmp/o13"
check "dry run commits nothing" eq "$(git rev-list --count origin/main..HEAD)" 0
check "dry run leaves the change uncommitted" test -n "$(git status --porcelain)"
check "dry run pushes nothing" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/x"
check "dry run calls nothing" eq "$(wc -l <"$FAKE_LOG" | tr -d ' ')" 0

# 14. usage errors
reset_state
w=$(newwork); cd "$w"
rc=$(run "$tmp/o14" --bogus)
check "unknown flag: exit 2" eq "$rc" 2
echo u >u.txt
rc=$(run "$tmp/o14b")
check "changes without a message: exit 2" eq "$rc" 2

echo "$pass passed, $fail failed"
exit $((fail > 0))
