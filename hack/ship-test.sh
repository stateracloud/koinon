#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/ship.sh: temp git repositories with a bare "origin", and `gh`, `gitleaks` and
# the hack/ helpers (needs-e2e.sh, claim.sh) as stubs. Nothing talks to GitHub.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
ship=$repo/hack/ship.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/koinon-test.XXXXXX") || exit 1 # explicit template: macOS mktemp -d ignores TMPDIR (koinon-private#466)
trap '[[ -n ${KEEP_TMP:-} ]] || rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
export PATH="$tmp/bin:$PATH" FAKE_LOG="$tmp/calls.log" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_TERMINAL_PROMPT=0 FAKE_NEEDS_RC="$tmp/needs.rc" FAKE_CLAIM_RC="$tmp/claim.rc" FAKE_LAB_RC="$tmp/lab.rc" FAKE_LAB_STATE="$tmp/lab.state"
: >"$FAKE_LOG"
echo 1 >"$FAKE_NEEDS_RC"
echo 0 >"$FAKE_CLAIM_RC"
echo 0 >"$FAKE_LAB_RC"
echo success >"$FAKE_LAB_STATE"

cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "gh $*" >>"$FAKE_LOG"
case "$1 $2" in
"pr list")
	if [[ " $* " == *" --state all "* ]]; then
		# the branch's PR lookup: apply the caller's --jq to FAKE_PR_JSON (a JSON array)
		q=""
		while [[ $# -gt 0 ]]; do [[ $1 == --jq ]] && q=$2; shift; done
		[[ -z ${FAKE_PR_JSON:-} ]] || jq -r "$q" <<<"$FAKE_PR_JSON"
	else
		echo "${FAKE_PR_URL:-}"
	fi
	;;
"pr view")
	if [[ " $* " == *" headRefOid,comments,commits "* ]]; then
		# the CodeRabbit guard (#184): apply the caller's --jq to FAKE_CR_JSON ({comments, commits}); no file = a read failure
		q=""
		while [[ $# -gt 0 ]]; do [[ $1 == --jq ]] && q=$2; shift; done
		[[ -n ${FAKE_CR_JSON:-} ]] || exit 1
		jq -r "$q" <<<"$FAKE_CR_JSON"
	else
		echo "${FAKE_PR_DRAFT:-true}"
	fi
	;;
"pr comment") exit "${FAKE_COMMENT_RC:-0}" ;;
"pr create") echo "${FAKE_PR_CREATE_URL:-https://example.invalid/o/r/pull/123}" ;;
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
# The lab client and the status reader (koinon-private#610): the gate's exit code and the visible status come from files.
cat >"$seed/hack/e2e-remote.sh" <<'SH'
#!/usr/bin/env bash
echo "e2e-remote $*" >>"$FAKE_LOG"
echo "lab gate output"
exit "$(cat "$FAKE_LAB_RC")"
SH
cat >"$seed/hack/go-test-lab.sh" <<'SH'
#!/usr/bin/env bash
echo "go-test-lab $*" >>"$FAKE_LOG"
cat "$FAKE_LAB_STATE"
SH
# The identity gate (koinon#320): ship runs the repository's own check against its own allowlist.
cp "$repo/hack/git-identity-check.sh" "$seed/hack/"
printf 'Test Dev <dev@example.invalid>\nadv <adv@example.invalid>\n' >"$seed/hack/git-identities.allowed"
chmod +x "$seed/hack/"*.sh
printf '# ADRs\n\n## Index\n' >"$seed/docs/adr/README.md"
printf '# 0001. First\n\n- Status: accepted\n' >"$seed/docs/adr/0001-first.md"
(cd "$seed" && hack/adr-index.sh >/dev/null && echo base >a.txt && git add -A && git commit -q -m seed && git remote add origin "$tmp/origin.git" && git push -q origin main)

newwork() { # prints the path of a fresh clone on branch feat/1-x with an identity
	local w
	# Fail hard (koinon-private#485): a failed mktemp or clone must never leave $w empty, or the git -C calls below
	# would run in the caller's own checkout and switch its branch.
	w=$(mktemp -d "$tmp/w.XXXXXX") || exit 1
	[[ -n $w && -d $w ]] || { echo "mktemp gave no directory" >&2; exit 1; }
	git clone -q "$tmp/origin.git" "$w" 2>/dev/null || { echo "clone into $w failed" >&2; exit 1; }
	[[ -d $w/.git ]] || { echo "$w is not a checkout" >&2; exit 1; }
	git -C "$w" config user.name "Test Dev"
	git -C "$w" config user.email dev@example.invalid
	git -C "$w" checkout -q -b feat/1-x
	echo "$w"
}
# shellcheck disable=SC2329 # called through check
branch_on_origin() { git --git-dir="$tmp/origin.git" rev-parse --verify -q "refs/heads/$1" >/dev/null; }
# koinon-private#485: when mktemp fails, newwork() must stop and leave the caller's checkout alone.
guard=$(mktemp -d "$tmp/guard.XXXXXX")
git -C "$guard" init -q -b keep
git -C "$guard" -c user.name=d -c user.email=d@example.invalid commit -q --allow-empty -m g
mkdir -p "$tmp/failbin"
printf '#!/bin/sh\nexit 1\n' >"$tmp/failbin/mktemp"
chmod +x "$tmp/failbin/mktemp"
mktemp_fail_rc=0
(cd "$guard" && PATH="$tmp/failbin:$PATH" && w=$(newwork)) >/dev/null 2>&1 || mktemp_fail_rc=$?
reset_state() { : >"$FAKE_LOG"; unset FAKE_PR_URL FAKE_PR_JSON FAKE_CR_JSON FAKE_COMMENT_RC FAKE_PR_CREATE_URL FAKE_PR_DRAFT FAKE_GITLEAKS_RC FAKE_BOARD_RC KOINON_ALLOWED_IDENTITIES KOINON_COMMIT_FOOTER || true; echo 1 >"$FAKE_NEEDS_RC"; echo 0 >"$FAKE_CLAIM_RC"; echo 0 >"$FAKE_LAB_RC"; echo success >"$FAKE_LAB_STATE"; }

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

# 1. identity refusal (hack/git-identity-check.sh commit, before any commit)
reset_state
w=$(newwork); cd "$w"
git config user.email t@example.invalid
echo one >f.txt
rc=$(run "$tmp/o1" -m "feat: x")
check "identity not allowed: exit 1" eq "$rc" 1
check "identity refusal is named" has "identity" "$tmp/o1"
check "identity refusal names the address" has "t@example.invalid" "$tmp/o1"
check "identity refusal commits nothing" eq "$(git rev-list --count origin/main..HEAD)" 0
check "identity refusal pushes nothing" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/1-x"

# 2. sign-off, footer, push, any-email notice
reset_state
w=$(newwork); cd "$w"
echo one >f.txt
export KOINON_COMMIT_FOOTER=$'Co-Authored-By: Bot <bot@example.invalid>\nClaude-Session: https://example.invalid/s'
rc=$(run "$tmp/o2" -m "feat: add f")
check "plain ship exits 0" eq "$rc" 0
check "commit line printed" has '^commit ok [0-9a-f]\{7,\}' "$tmp/o2"
check "identity line printed" has '^identity dev@example.invalid$' "$tmp/o2"
git log -1 --format=%B >"$tmp/m2"
check "sign-off present" has '^Signed-off-by: Test Dev <dev@example.invalid>$' "$tmp/m2"
check "footer appended" has '^Claude-Session: https://example.invalid/s$' "$tmp/m2"
check "gitleaks ran over origin/main..HEAD" has 'gitleaks git --redact --no-banner --log-opts=origin/main..HEAD' "$FAKE_LOG"
check "gitleaks line counts commits" has '^gitleaks ok (1 commits)$' "$tmp/o2"
check "pushed line" has '^pushed feat/1-x$' "$tmp/o2"
check "branch reached origin" branch_on_origin feat/1-x
check "no PR without --pr-title" hasnt 'gh pr \(create\|edit\|merge\)' "$FAKE_LOG"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x

# 4. blob refusal
reset_state
w=$(newwork); cd "$w"
dd if=/dev/zero of=big.bin bs=1048576 count=6 2>/dev/null
rc=$(run "$tmp/o4" -m "feat: big")
check "6 MiB file: exit 1" eq "$rc" 1
check "blob refusal names the path" has 'big.bin' "$tmp/o4"
check "blob refusal does not commit" eq "$(git rev-list --count origin/main..HEAD)" 0
check "blob refusal does not push" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/1-x"
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
check "pushed branch contains the new main" git --git-dir="$tmp/origin.git" merge-base --is-ancestor "$(git -C "$adv" rev-parse HEAD)" refs/heads/feat/1-x
check "merge commit exists" eq "$(git rev-list --merges --count origin/main..HEAD)" 1
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
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
check "conflict pushes nothing" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/1-x"
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
check "ADR README: pushed" branch_on_origin feat/1-x
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
rm -rf "$adv"

# 8b. a hook that refuses the merge commit is reported as such, not as a conflict
reset_state
w=$(newwork); cd "$w"
git clone -q "$tmp/origin.git" "$adv"
git -C "$adv" config user.name adv && git -C "$adv" config user.email adv@example.invalid
echo more2 >"$adv/c.txt" && git -C "$adv" add -A && git -C "$adv" commit -q -m "main moves again" && git -C "$adv" push -q origin main
mkdir -p "$tmp/hooks"
printf '#!/bin/sh\necho "identity refused by hook" >&2\nexit 1\n' >"$tmp/hooks/pre-merge-commit"
chmod +x "$tmp/hooks/pre-merge-commit"
git config core.hooksPath "$tmp/hooks"
echo mine2 >mine2.txt
rc=$(run "$tmp/o8b" -m "feat: mine2")
check "hook-refused merge: exit 1" eq "$rc" 1
check "hook-refused merge shows the hook output" has 'identity refused by hook' "$tmp/o8b"
check "hook-refused merge is not called a conflict" hasnt 'merge conflict in' "$tmp/o8b"
check "hook-refused merge leaves no merge in progress" bash -c "! git rev-parse -q --verify MERGE_HEAD"
check "hook-refused merge pushes nothing" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/1-x"
rm -rf "$adv"

# 9. gitleaks failure stops before the push
reset_state
w=$(newwork); cd "$w"
echo s >s.txt
export FAKE_GITLEAKS_RC=1
rc=$(run "$tmp/o9" -m "feat: s")
check "gitleaks failure: exit 1" eq "$rc" 1
check "gitleaks failure is named" has 'gitleaks' "$tmp/o9"
check "gitleaks failure: no push" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/1-x"

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
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x

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
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x

# 11c. (koinon-private#392) --e2e: the label goes on an open PR before the push, on a new PR at creation
reset_state
unset FAKE_PR_URL
w=$(newwork); cd "$w"
echo e3 >e3.txt
rc=$(run "$tmp/o11c" -m "feat: e3" --pr-title "t" --pr-body "$tmp/body.md" --e2e)
check "--e2e new PR: exit 0" eq "$rc" 0
check "--e2e new PR: created with the label" has 'gh pr create .*--label e2e' "$FAKE_LOG"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
reset_state
export FAKE_PR_URL=https://example.invalid/o/r/pull/123
echo e4 >e4.txt
rc=$(run "$tmp/o11d" -m "feat: e4" --pr-title "t" --pr-body "$tmp/body.md" --e2e)
check "--e2e open PR: exit 0" eq "$rc" 0
check "--e2e open PR: label added" has 'gh pr edit .* --add-label e2e' "$FAKE_LOG"
check "--e2e open PR: label step before the push" has '^label e2e$' "$tmp/o11d"
unset FAKE_PR_URL
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x

# 12. claim review, and a lost claim
reset_state
w=$(newwork); cd "$w"
echo c >c.txt
rc=$(run "$tmp/o12" -m "feat: c" --review 7,8)
check "review: exit 0" eq "$rc" 0
check "review lines" has '^review ok #8$' "$tmp/o12"
check "claim.sh review called per issue" has '^claim review 7$' "$FAKE_LOG"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
reset_state
echo 3 >"$FAKE_CLAIM_RC"
w=$(newwork); cd "$w"
echo c >c.txt
rc=$(run "$tmp/o12b" -m "feat: c" --review 7)
check "claim lost: exit 3" eq "$rc" 3
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x

# 13. dry run does nothing
reset_state
w=$(newwork); cd "$w"
echo d >d.txt
rc=$(run "$tmp/o13" -m "feat: d" --pr-title t --pr-body "$tmp/body.md" --ready --auto-merge --review 5 --dry-run)
check "dry run: exit 0" eq "$rc" 0
check "dry run prints steps" has '^dry-run: would' "$tmp/o13"
check "dry run commits nothing" eq "$(git rev-list --count origin/main..HEAD)" 0
check "dry run leaves the change uncommitted" test -n "$(git status --porcelain)"
check "dry run pushes nothing" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/1-x"
check "dry run calls nothing" eq "$(wc -l <"$FAKE_LOG" | tr -d ' ')" 0

# 14. usage errors
reset_state
w=$(newwork); cd "$w"
rc=$(run "$tmp/o14" --bogus)
check "unknown flag: exit 2" eq "$rc" 2
echo u >u.txt
rc=$(run "$tmp/o14b")
check "changes without a message: exit 2" eq "$rc" 2

# 15. (koinon-private#431) the branch's PR: merged or closed refuses before the push; auto-merge on warns; --hold
reset_state
w=$(newwork); cd "$w"
echo m >m.txt
export FAKE_PR_JSON='[{"number":427,"state":"MERGED","autoMergeRequest":null}]'
rc=$(run "$tmp/o15" -m "fix: m")
check "merged PR: exit 1" eq "$rc" 1
check "merged PR is named" has 'PR #427 for feat/1-x is MERGED' "$tmp/o15"
check "merged PR: says how to continue" has 'hack/wt.sh new <name> --from feat/1-x --branch <new-branch>' "$tmp/o15"
check "merged PR: nothing pushed" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/1-x"
check "merged PR: gitleaks not reached" hasnt '^gitleaks' "$FAKE_LOG"
export FAKE_PR_JSON='[{"number":428,"state":"CLOSED","autoMergeRequest":null}]'
rc=$(run "$tmp/o15b")
check "closed PR: exit 1" eq "$rc" 1
check "closed PR is named" has 'is CLOSED' "$tmp/o15b"
# an older merged PR plus a newer open one: the open one counts
export FAKE_PR_JSON='[{"number":427,"state":"MERGED","autoMergeRequest":null},{"number":430,"state":"OPEN","autoMergeRequest":null}]'
rc=$(run "$tmp/o15c")
check "merged and open PR on the branch: pushes" eq "$rc" 0
check "open PR without auto-merge: no warning" hasnt 'auto-merge on' "$tmp/o15c"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
reset_state
w=$(newwork); cd "$w"
echo n >n.txt
export FAKE_PR_JSON='[{"number":431,"state":"OPEN","autoMergeRequest":{"mergeMethod":"MERGE"}}]'
rc=$(run "$tmp/o15d" -m "fix: n")
check "auto-merge on: still pushes" eq "$rc" 0
check "auto-merge on: warning" has 'warning: PR #431 has auto-merge on' "$tmp/o15d"
check "auto-merge on: not turned off" hasnt 'disable-auto' "$FAKE_LOG"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
reset_state
w=$(newwork); cd "$w"
echo o >o.txt
export FAKE_PR_JSON='[{"number":431,"state":"OPEN","autoMergeRequest":{"mergeMethod":"MERGE"}}]'
rc=$(run "$tmp/o15e" -m "fix: o" --hold)
check "--hold: exit 0" eq "$rc" 0
check "--hold: gh pr merge --disable-auto" has 'gh pr merge 431 --disable-auto' "$FAKE_LOG"
check "--hold: says how to turn it back on" has 'gh pr merge 431 --auto --merge' "$tmp/o15e"
check "--hold: no warning" hasnt 'warning' "$tmp/o15e"
check "--hold: auto-merge off before the push" bash -c "awk '/disable-auto/{a=NR} END{exit !a}' $FAKE_LOG"
check "--hold: pushed" branch_on_origin feat/1-x
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
unset FAKE_PR_JSON

check "mktemp failure: newwork() fails" test "$mktemp_fail_rc" -ne 0
check "mktemp failure: caller's checkout branch unchanged" eq "$(git -C "$guard" branch --show-current)" keep

# koinon-private#485: ship.sh refuses a branch that is not <type>/<N>-<slug>
reset_state
w=$(newwork); cd "$w"
git checkout -q -b stray
echo s >s.txt
rc=$(run "$tmp/o16" -m "fix: s")
check "stray branch name: refused" eq "$rc" 1
check "stray branch name: message" has 'is not <type>/<N>-<slug>' "$tmp/o16"
check "stray branch name: nothing pushed" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/stray"

# koinon-private#485: unnumbered branches (exempt work) need --no-issue
reset_state
w=$(newwork); cd "$w"
git checkout -q -b chore/light-lane-cpu
echo c >c.txt
rc=$(run "$tmp/o17" -m "chore: c")
check "unnumbered branch without --no-issue: refused" eq "$rc" 1
check "unnumbered branch refusal names --no-issue" has 'no-issue' "$tmp/o17"
check "unnumbered branch refused: nothing pushed" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/chore/light-lane-cpu"
rc=$(run "$tmp/o18" --no-issue -m "chore: c")
check "unnumbered branch with --no-issue: exit 0" eq "$rc" 0
check "unnumbered branch with --no-issue: pushed" branch_on_origin chore/light-lane-cpu
git checkout -q -b stray
echo t >t.txt
rc=$(run "$tmp/o19" --no-issue -m "chore: t")
check "--no-issue still refuses a stray name" eq "$rc" 1
# 20. (koinon-private#610) --lab-go-test: the lab gate runs after the push and before the pull request is marked ready
reset_state
w=$(newwork); cd "$w"
echo l >l.txt
rc=$(run "$tmp/o20" -m "feat: l" --pr-title t --pr-body "$tmp/body.md" --lab-go-test)
check "--lab-go-test without --ready: usage error" eq "$rc" 2
check "...nothing pushed" bash -c "! git --git-dir=$tmp/origin.git rev-parse -q --verify refs/heads/feat/1-x"
rc=$(run "$tmp/o20b" -m "feat: l" --pr-title t --pr-body "$tmp/body.md" --ready --lab-go-test --dry-run)
check "--lab-go-test dry run: exit 0" eq "$rc" 0
check "dry run names the lab gate" has 'would run the go-test gate on the lab' "$tmp/o20b"
check "dry run runs no gate" hasnt 'e2e-remote' "$FAKE_LOG"
reset_state
rc=$(run "$tmp/o20c" -m "feat: l" --pr-title t --pr-body "$tmp/body.md" --ready --lab-go-test)
check "lab go-test green: exit 0" eq "$rc" 0
check "the gate is go-test on the lab queue" has 'e2e-remote --argo --gate go-test' "$FAKE_LOG"
check "the status is read on the pushed head" has "go-test-lab state $(git rev-parse HEAD)" "$FAKE_LOG"
check "a new PR is created as a draft when the lab runs first" has 'gh pr create .*--draft' "$FAKE_LOG"
check "lab go-test ok line" has '^lab go-test ok' "$tmp/o20c"
check "the lab ran before the pull request was marked ready" bash -c "awk '/e2e-remote/ { l = NR } /^gh pr ready/ { r = NR } END { exit !(l && r && l < r) }' $FAKE_LOG"
check "ready happened" has '^ready$' "$tmp/o20c"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
for c in 75 76 78 2; do
	reset_state
	echo "$c" >"$FAKE_LAB_RC"
	w=$(newwork); cd "$w"
	echo "l$c" >"l$c.txt"
	rc=$(run "$tmp/o21-$c" -m "feat: l" --pr-title t --pr-body "$tmp/body.md" --ready --lab-go-test)
	check "lab exit $c: falls back to the hosted runner, exit 0" eq "$rc" 0
	check "lab exit $c: the reason is printed" has "lab go-test:.*go-test runs on the hosted runner" "$tmp/o21-$c"
	check "lab exit $c: skipped line" has "^lab go-test skipped (exit $c)" "$tmp/o21-$c"
	check "lab exit $c: still marked ready" has '^ready$' "$tmp/o21-$c"
	check "lab exit $c: no status read" hasnt 'go-test-lab state' "$FAKE_LOG"
	git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
done
# an already ready PR: the push started CI, the lab run would come too late
reset_state
export FAKE_PR_URL=https://example.invalid/o/r/pull/123 FAKE_PR_DRAFT=false
w=$(newwork); cd "$w"
echo lr >lr.txt
rc=$(run "$tmp/o21r" -m "feat: l" --pr-title t --pr-body "$tmp/body.md" --ready --lab-go-test)
check "already ready PR: exit 0" eq "$rc" 0
check "already ready PR: the lab does not run" hasnt 'e2e-remote' "$FAKE_LOG"
check "already ready PR: says why and how" has 'PR already ready.*gh pr ready --undo' "$tmp/o21r"
check "already ready PR: skipped line" has '^lab go-test skipped (PR already ready)' "$tmp/o21r"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
reset_state
echo 7 >"$FAKE_LAB_RC"
w=$(newwork); cd "$w"
echo l7 >l7.txt
rc=$(run "$tmp/o21f" -m "feat: l" --pr-title t --pr-body "$tmp/body.md" --ready --lab-go-test)
check "a gate's own failure status (rc 7) stops the ship" eq "$rc" 1
check "...without marking ready" hasnt '^ready$' "$tmp/o21f"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
reset_state
echo 1 >"$FAKE_LAB_RC"
w=$(newwork); cd "$w"
echo lf >lf.txt
rc=$(run "$tmp/o22" -m "feat: l" --pr-title t --pr-body "$tmp/body.md" --ready --lab-go-test)
check "lab go-test failed: exit 1" eq "$rc" 1
check "...says so" has 'lab go-test failed' "$tmp/o22"
check "...and the pull request is not marked ready" hasnt '^ready$' "$tmp/o22"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
reset_state
echo none >"$FAKE_LAB_STATE"
w=$(newwork); cd "$w"
echo ln >ln.txt
rc=$(run "$tmp/o23" -m "feat: l" --pr-title t --pr-body "$tmp/body.md" --ready --lab-go-test)
check "lab passed but no status visible: warns and goes on, exit 0" eq "$rc" 0
check "...the warning names the cost" has 'go-test-lab is not visible' "$tmp/o23"
check "...skipped line" has '^lab go-test skipped (no status)' "$tmp/o23"
check "...ready happened" has '^ready$' "$tmp/o23"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x

# 24. (koinon-private#675) --ready on an existing draft PR marks it ready BEFORE the push, so one head never gets a
# synchronize run and a ready_for_review run seconds apart
reset_state
export FAKE_PR_URL=https://example.invalid/o/r/pull/123 FAKE_PR_DRAFT=true
export FAKE_PR_JSON='[{"number":123,"state":"OPEN","autoMergeRequest":null}]'
w=$(newwork); cd "$w"
# shellcheck disable=SC2016 # $FAKE_LOG expands when the hook runs
printf '#!/bin/sh\necho "git-push" >>"$FAKE_LOG"\n' >.git/hooks/pre-push
chmod +x .git/hooks/pre-push
echo rd >rd.txt
rc=$(run "$tmp/o24" -m "feat: r" --pr-title t --pr-body "$tmp/body.md" --ready)
check "ready before push: exit 0" eq "$rc" 0
check "ready before push: pushed" has 'git-push' "$FAKE_LOG"
check "ready before push: gh pr ready ran before the push" bash -c "awk '/^gh pr ready/ { r = NR } /^git-push/ { p = NR } END { exit !(r && p && r < p) }' $FAKE_LOG"
check "ready before push: marked ready exactly once" eq "$(grep -c '^gh pr ready' "$FAKE_LOG")" 1
check "ready before push: ready line" has '^ready$' "$tmp/o24"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
# an already ready PR is not marked again; with --lab-go-test the order stays push, lab, ready
reset_state
export FAKE_PR_URL=https://example.invalid/o/r/pull/123 FAKE_PR_DRAFT=false
export FAKE_PR_JSON='[{"number":123,"state":"OPEN","autoMergeRequest":null}]'
w=$(newwork); cd "$w"
# shellcheck disable=SC2016 # $FAKE_LOG expands when the hook runs
printf '#!/bin/sh\necho "git-push" >>"$FAKE_LOG"\n' >.git/hooks/pre-push
chmod +x .git/hooks/pre-push
echo rd2 >rd2.txt
rc=$(run "$tmp/o24b" -m "feat: r" --pr-title t --pr-body "$tmp/body.md" --ready)
check "already ready PR: exit 0" eq "$rc" 0
# ship.sh marks ready before the push only a draft; an already ready PR gets its one idempotent
# `gh pr ready` in step 7, after the push
check "already ready PR: gh pr ready exactly once" eq "$(grep -c '^gh pr ready' "$FAKE_LOG")" 1
check "already ready PR: gh pr ready only after the push" bash -c "awk '/^gh pr ready/ { r = NR } /^git-push/ { p = NR } END { exit !(r && p && p < r) }' $FAKE_LOG"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
reset_state
export FAKE_PR_URL=https://example.invalid/o/r/pull/123 FAKE_PR_DRAFT=true
export FAKE_PR_JSON='[{"number":123,"state":"OPEN","autoMergeRequest":null}]'
w=$(newwork); cd "$w"
# shellcheck disable=SC2016 # $FAKE_LOG expands when the hook runs
printf '#!/bin/sh\necho "git-push" >>"$FAKE_LOG"\n' >.git/hooks/pre-push
chmod +x .git/hooks/pre-push
echo rd3 >rd3.txt
rc=$(run "$tmp/o24c" -m "feat: r" --pr-title t --pr-body "$tmp/body.md" --ready --lab-go-test)
check "lab: exit 0" eq "$rc" 0
check "lab: ready only after the push and the lab" bash -c "awk '/^git-push/ { p = NR } /e2e-remote/ { l = NR } /^gh pr ready/ { r = NR } END { exit !(p && l && r && p < l && l < r) }' $FAKE_LOG"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x

# 25. (koinon-private#675) the push fails after the PR was marked ready: the PR is put back to draft, and the error says so
reset_state
export FAKE_PR_URL=https://example.invalid/o/r/pull/123 FAKE_PR_DRAFT=true
export FAKE_PR_JSON='[{"number":123,"state":"OPEN","autoMergeRequest":null}]'
w=$(newwork); cd "$w"
printf '#!/bin/sh\nexit 1\n' >.git/hooks/pre-push
chmod +x .git/hooks/pre-push
echo pf >pf.txt
rc=$(run "$tmp/o25" -m "feat: r" --pr-title t --pr-body "$tmp/body.md" --ready)
check "push failed after ready: exit 1" eq "$rc" 1
check "push failed after ready: marked ready first" has '^gh pr ready 123' "$FAKE_LOG"
check "push failed after ready: PR put back to draft" has '^gh pr ready --undo 123' "$FAKE_LOG"
check "push failed after ready: the error says so" has 'put back to draft' "$tmp/o25"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x 2>/dev/null || true

# 26. (#184) --ready asks CodeRabbit for a review once per head, only on stateracloud/koinon; a failure only warns
cr_url=https://github.com/stateracloud/koinon/pull/123
cr_ship() { # <tag> <cr json or empty> [comment rc] [again]: a fresh branch, ready PR, one new commit
	reset_state
	export FAKE_PR_URL=$cr_url FAKE_PR_DRAFT=true FAKE_PR_JSON='[{"number":123,"state":"OPEN","autoMergeRequest":null}]'
	[[ -z $2 ]] || export FAKE_CR_JSON=$2
	[[ -z ${3:-} ]] || export FAKE_COMMENT_RC=$3
	w=$(newwork); cd "$w"
	echo "$1" >"$1.txt"
	if [[ -n ${4:-} ]]; then # not a new head: ship once, then ship again with nothing new
		run "$tmp/o26$1-first" -m "feat: $1" --pr-title t --pr-body "$tmp/body.md" --ready >/dev/null
		: >"$FAKE_LOG"
	fi
	run "$tmp/o26$1" -m "feat: $1" --pr-title t --pr-body "$tmp/body.md" --ready
	git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
}
c_old='{"headRefOid":"h1","commits":[{"oid":"h1","committedDate":"2026-01-02T00:00:00Z"}],"comments":[{"body":"@coderabbitai review","createdAt":"2026-01-01T00:00:00Z"}]}'
c_new='{"headRefOid":"h1","commits":[{"oid":"h1","committedDate":"2026-01-01T00:00:00Z"}],"comments":[{"body":"@coderabbitai review\n","createdAt":"2026-01-02T00:00:00Z"}]}'
c_none='{"headRefOid":"h1","commits":[{"oid":"h1","committedDate":"2026-01-01T00:00:00Z"}],"comments":[{"body":"looks good","createdAt":"2026-01-02T00:00:00Z"}]}'
rc=$(cr_ship none "$c_none")
check "coderabbit: exit 0" eq "$rc" 0
check "coderabbit: posts the review request" has '^gh pr comment https://github.com/stateracloud/koinon/pull/123 --body @coderabbitai review$' "$FAKE_LOG"
check "coderabbit: says so" has '^coderabbit review requested$' "$tmp/o26none"
rc=$(cr_ship fresh "$c_new")
check "coderabbit: a just-pushed head posts even with a newer-dated request comment" has '^gh pr comment ' "$FAKE_LOG"
rc=$(cr_ship old "$c_old" "" again)
check "coderabbit: unchanged head, request dated before the head commit: posts" has '^gh pr comment ' "$FAKE_LOG"
rc=$(cr_ship new "$c_new" "" again)
check "coderabbit: already asked on this head: exit 0" eq "$rc" 0
check "coderabbit: already asked on this head: no second comment" hasnt '^gh pr comment ' "$FAKE_LOG"
check "coderabbit: already asked on this head: says so" has '^coderabbit review already requested on this head$' "$tmp/o26new"
rc=$(cr_ship fail "$c_none" 1)
check "coderabbit: a failed comment never fails ship" eq "$rc" 0
check "coderabbit: a failed comment warns" has 'could not post @coderabbitai review' "$tmp/o26fail"
rc=$(cr_ship rd "" "" again)
check "coderabbit: unreadable comments never fail ship" eq "$rc" 0
check "coderabbit: unreadable comments warn and post nothing" bash -c "grep -q 'could not read the comments' $tmp/o26rd && ! grep -q '^gh pr comment ' $FAKE_LOG"
# another repository, and a pull request that is not marked ready: nothing is asked
reset_state
export FAKE_PR_URL=https://example.invalid/o/r/pull/123 FAKE_PR_JSON='[{"number":123,"state":"OPEN","autoMergeRequest":null}]' FAKE_CR_JSON=$c_none
w=$(newwork); cd "$w"
echo other >other.txt
rc=$(run "$tmp/o26o" -m "feat: o" --pr-title t --pr-body "$tmp/body.md" --ready)
check "coderabbit: other repository: exit 0" eq "$rc" 0
check "coderabbit: other repository: no comment" hasnt '^gh pr comment ' "$FAKE_LOG"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x
reset_state
export FAKE_PR_URL=$cr_url FAKE_PR_JSON='[{"number":123,"state":"OPEN","autoMergeRequest":null}]' FAKE_CR_JSON=$c_none
w=$(newwork); cd "$w"
echo nr >nr.txt
rc=$(run "$tmp/o26n" -m "feat: n" --pr-title t --pr-body "$tmp/body.md")
check "coderabbit: without --ready: exit 0" eq "$rc" 0
check "coderabbit: without --ready: no comment" hasnt '^gh pr comment ' "$FAKE_LOG"
git --git-dir="$tmp/origin.git" update-ref -d refs/heads/feat/1-x

echo "$pass passed, $fail failed"
exit $((fail > 0))
