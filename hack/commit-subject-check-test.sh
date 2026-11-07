#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/commit-subject-check.sh on a scratch repository: conforming subjects pass,
# non-conforming ones fail, and merge commits, Dependabot and `git revert` subjects are exempt.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
script=$repo/hack/commit-subject-check.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/commit-subject-check-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=Tester GIT_AUTHOR_EMAIL=tester@example.invalid
export GIT_COMMITTER_NAME=Tester GIT_COMMITTER_EMAIL=tester@example.invalid

git init -q -b main "$tmp/r" || exit 1
cd "$tmp/r" || exit 1
git commit -q --allow-empty -m "chore: base"
base=$(git rev-parse HEAD)

fail=0
n=0
# commit <subject> [author name] [author email]
commit() {
	n=$((n + 1))
	GIT_AUTHOR_NAME=${2:-Tester} GIT_AUTHOR_EMAIL=${3:-tester@example.invalid} \
		git commit -q --allow-empty -m "$1" -m "body $n"
}
# expect <name> <0|1>: run the script over base..HEAD and compare the exit code
expect() {
	local out rc
	out=$("$script" "$base" HEAD 2>&1)
	rc=$?
	if [[ $rc -eq $2 ]]; then echo "ok   $1"; else echo "FAIL $1: exit $rc, want $2"; echo "$out"; fail=1; fi
	base=$(git rev-parse HEAD)
}

for s in 'feat: add x' 'fix(api): handle y' 'feat(web)!: drop z' 'ci: pin actions' 'revert: undo q' 'perf(db): index'; do
	commit "$s"
done
expect "conforming subjects" 0

commit "Update README"
expect "plain subject fails" 1

commit "feature: add x"
expect "unknown type fails" 1

commit "fix:missing space"
expect "missing space after colon fails" 1

commit "fix(scope):"
expect "empty summary fails" 1

commit "Bump foo from 1 to 2" "dependabot[bot]" "49699333+dependabot[bot]@users.noreply.github.com"
expect "dependabot is exempt" 0

commit "Bump foo from 1 to 2" "dependabot[bot]" "someone@example.invalid"
expect "dependabot name with another email is not exempt" 1

commit 'Revert "feat: add x"'
expect "git revert subject is exempt" 0

git checkout -q -b side
commit "feat: on side"
git checkout -q main
commit "fix: on main"
git merge -q --no-ff -m "Merge branch 'side'" side
expect "merge commit is exempt" 0

# A range with no non-merge commits (base == head) has nothing to check and passes.
out=$("$script" HEAD HEAD 2>&1)
rc=$?
if [[ $rc -eq 0 ]]; then echo "ok   empty range passes"; else echo "FAIL empty range: exit $rc, want 0"; echo "$out"; fail=1; fi

# A failing git rev-list (unknown revision) must fail closed, not pass on an empty list.
out=$("$script" "$base" does-not-exist 2>&1)
rc=$?
if [[ $rc -ne 0 && $out == *"rev-list"* ]]; then echo "ok   failing rev-list fails closed"; else echo "FAIL failing rev-list: exit $rc, want non-zero with a rev-list message"; echo "$out"; fail=1; fi

# A rev-list warning on stderr (an ambiguous refname) is not read as a commit: the range still passes.
commit "fix: ambiguous head"
git tag amb
git branch amb
out=$("$script" "$base" amb 2>&1)
rc=$?
if [[ $rc -eq 0 && $out == *ambiguous* ]]; then echo "ok   rev-list warning is not a commit"; else echo "FAIL rev-list warning: exit $rc, want 0 with the warning in the log"; echo "$out"; fail=1; fi

[[ $fail -eq 0 ]] && echo "commit-subject-check tests passed"
exit $fail
