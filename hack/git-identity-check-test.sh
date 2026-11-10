#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/git-identity-check.sh and its hooks on a scratch repository: a worktree with the
# hooks installed refuses a commit and a merge commit under another identity, accepts the project
# identity, and `range` (CI) fails a commit whose author or committer is outside the allowed list.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/git-identity-check-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
unset GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL KOINON_ALLOWED_IDENTITIES KOINON_SKIP_IDENTITY_CHECK CI

fail=0
check() { # check <name> <want-rc> <got-rc> [output]
	if [[ $2 -eq $3 ]]; then echo "ok   $1"; else echo "FAIL $1: exit $3, want $2"; [[ -n ${4:-} ]] && echo "$4"; fail=1; fi
}

# A scratch repository with this repo's hack/ copied in (the hooks resolve the script relative to themselves).
git init -q -b main "$tmp/r" || exit 1
cd "$tmp/r" || exit 1
mkdir hack && cp -R "$repo/hack/githooks" "$repo/hack/git-identity-check.sh" "$repo/hack/git-identities.allowed" hack/
setid() { git config user.name "$1"; git config user.email "$2"; }
setid procoduck coder@procodus.dev
git add hack && git commit -q -m "chore: base" --no-verify
base=$(git rev-parse HEAD)

# install
out=$(hack/git-identity-check.sh install 2>&1); check "install sets the hooks" 0 $? "$out"
[[ $(git config --get core.hooksPath) == hack/githooks ]] || { echo "FAIL core.hooksPath not set"; fail=1; }

# the hooks
setid procoduck coder@procodus.dev
out=$(git commit -q --allow-empty -m "feat: ok" 2>&1); check "project identity commits" 0 $? "$out"

setid "Some Person" some.person@example.invalid
out=$(git commit -q --allow-empty -m "feat: bad" 2>&1); rc=$?
check "another identity is refused" 1 $rc "$out"
[[ $out == *"not in the allowed list"* ]] || { echo "FAIL refusal message missing"; echo "$out"; fail=1; }

setid procoduck coder@procodus.dev
out=$(GIT_COMMITTER_NAME="Some Person" GIT_COMMITTER_EMAIL=some.person@example.invalid git commit -q --allow-empty -m "feat: bad committer" 2>&1)
check "a foreign committer is refused" 1 $? "$out"

out=$(GIT_AUTHOR_NAME="Some Person" GIT_AUTHOR_EMAIL=some.person@example.invalid git commit -q --allow-empty -m "feat: bad author" 2>&1)
check "a foreign author is refused" 1 $? "$out"

setid "Some Person" some.person@example.invalid
out=$(KOINON_SKIP_IDENTITY_CHECK=1 git commit -q --allow-empty -m "feat: skipped" 2>&1)
check "KOINON_SKIP_IDENTITY_CHECK=1 skips the hook" 0 $? "$out"
out=$(KOINON_SKIP_IDENTITY_CHECK=1 CI=true git commit -q --allow-empty -m "feat: skipped in ci" 2>&1)
check "the skip is ignored in CI" 1 $? "$out"

out=$(KOINON_ALLOWED_IDENTITIES='Some Person <some.person@example.invalid>' git commit -q --allow-empty -m "feat: own identity" 2>&1)
check "KOINON_ALLOWED_IDENTITIES allows another contributor" 0 $? "$out"

out=$(KOINON_ALLOWED_IDENTITIES='# only a comment' git commit -q --allow-empty -m "feat: empty list" 2>&1)
check "an empty list fails closed" 1 $? "$out"

setid "Some Person" 12345+someone@users.noreply.github.com
out=$(git commit -q --allow-empty -m "feat: noreply" 2>&1)
check "a GitHub noreply address is allowed" 0 $? "$out"

# merge commits: the case that started this (a personal name on a merge of origin/main)
setid procoduck coder@procodus.dev
git checkout -q -b side "$base"
git commit -q --allow-empty -m "fix: on side"
git checkout -q main
git commit -q --allow-empty -m "fix: on main"
setid "Some Person" some.person@example.invalid
out=$(git merge --no-ff -m "Merge branch 'side'" side 2>&1)
check "a merge commit under another identity is refused" 1 $? "$out"
git merge --abort 2>/dev/null
setid procoduck coder@procodus.dev
out=$(git merge --no-ff -q -m "Merge branch 'side'" side 2>&1)
check "a merge commit under the project identity succeeds" 0 $? "$out"

# range (CI), no hooks involved
git config --unset core.hooksPath
setid procoduck coder@procodus.dev
base=$(git rev-parse HEAD)
git commit -q --allow-empty -m "feat: fine"
out=$(hack/git-identity-check.sh range "$base" HEAD 2>&1); check "range: project identity passes" 0 $? "$out"

GIT_AUTHOR_NAME="Some Person" GIT_AUTHOR_EMAIL=some.person@example.invalid git commit -q --allow-empty -m "feat: foreign author"
out=$(hack/git-identity-check.sh range "$base" HEAD 2>&1); rc=$?
check "range: a foreign author fails" 1 $rc "$out"
[[ $out == *"Some Person"* ]] || { echo "FAIL range output does not name the identity"; echo "$out"; fail=1; }
base=$(git rev-parse HEAD)

GIT_COMMITTER_NAME="Some Person" GIT_COMMITTER_EMAIL=some.person@example.invalid git commit -q --allow-empty -m "feat: foreign committer"
out=$(hack/git-identity-check.sh range "$base" HEAD 2>&1); check "range: a foreign committer fails" 1 $? "$out"
base=$(git rev-parse HEAD)

# a foreign merge commit (not only non-merge commits) fails
git checkout -q -b side2 "$base"
git commit -q --allow-empty -m "fix: side2"
git checkout -q main
git commit -q --allow-empty -m "fix: main2"
GIT_AUTHOR_NAME="Some Person" GIT_AUTHOR_EMAIL=some.person@example.invalid GIT_COMMITTER_NAME="Some Person" GIT_COMMITTER_EMAIL=some.person@example.invalid \
	git merge -q --no-ff -m "Merge branch 'side2'" side2
out=$(hack/git-identity-check.sh range "$base" HEAD 2>&1); check "range: a merge commit under another identity fails" 1 $? "$out"
base=$(git rev-parse HEAD)

git commit -q --allow-empty -m "Bump x" --author="dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>"
out=$(hack/git-identity-check.sh range "$base" HEAD 2>&1); check "range: dependabot passes" 0 $? "$out"

# the list is read from the BASE commit: a head that widens it does not help its own commits
base=$(git rev-parse HEAD)
printf 'Some Person <some.person@example.invalid>\n' >>hack/git-identities.allowed
git add hack/git-identities.allowed
git commit -q -m "chore: widen the list" --no-verify
GIT_AUTHOR_NAME="Some Person" GIT_AUTHOR_EMAIL=some.person@example.invalid git commit -q --allow-empty -m "feat: relies on the widened list"
out=$(hack/git-identity-check.sh range "$base" HEAD 2>&1); check "range: a head that widens the list is still refused" 1 $? "$out"
out=$(KOINON_ALLOWED_IDENTITIES='Some Person <some.person@example.invalid>' hack/git-identity-check.sh range "$base" HEAD 2>&1)
check "range: KOINON_ALLOWED_IDENTITIES is not honoured" 1 $? "$out"
# once the widened list is on the base, the next pull request may use it
base2=$(git rev-parse HEAD)
GIT_AUTHOR_NAME="Some Person" GIT_AUTHOR_EMAIL=some.person@example.invalid git commit -q --allow-empty -m "feat: uses the merged list"
out=$(hack/git-identity-check.sh range "$base2" HEAD 2>&1); check "range: a line already on the base is allowed" 0 $? "$out"

# first add: the base has no list, so the head's (newly added) list is used
git checkout -q --orphan firstadd
cp hack/git-identity-check.sh "$tmp/chk.sh"
git rm -rq --cached . && rm -rf hack
git commit -q --allow-empty -m "chore: no list yet" --no-verify
nobase=$(git rev-parse HEAD)
mkdir hack && cp "$repo/hack/git-identities.allowed" hack/
git add hack && git commit -q -m "chore: add the list" --no-verify
out=$("$tmp/chk.sh" range "$nobase" HEAD 2>&1); check "range: first add falls back to the head's list" 0 $? "$out"
[[ $out == *"newly added"* ]] || { echo "FAIL fallback not announced"; echo "$out"; fail=1; }
git rm -q hack/git-identities.allowed && git commit -q -m "chore: no list" --no-verify
out=$("$tmp/chk.sh" range "$nobase" HEAD 2>&1); check "range: no list at base or head fails closed" 2 $? "$out"
git checkout -q -f main

out=$(hack/git-identity-check.sh range "$base" nope 2>&1); check "range: an unknown revision fails closed" 2 $? "$out"
out=$(hack/git-identity-check.sh range 2>&1); check "range: misuse exits 2" 2 $? "$out"
out=$(hack/git-identity-check.sh bogus 2>&1); check "unknown mode exits 2" 2 $? "$out"

# personal hooks: core.hooksPath replaces .git/hooks, so the guard hooks chain to the common dir's hook
mkdir -p .git/hooks
printf '#!/bin/sh\necho ran >> "%s/personal-pre-commit"\n' "$tmp" >.git/hooks/pre-commit
printf '#!/bin/sh\necho ran >> "%s/personal-pre-merge-commit"\nexit 1\n' "$tmp" >.git/hooks/pre-merge-commit
chmod +x .git/hooks/pre-commit .git/hooks/pre-merge-commit
setid procoduck coder@procodus.dev
out=$(git commit -q --allow-empty -m "feat: chained" 2>&1); check "chain: a commit still succeeds with a personal pre-commit" 0 $? "$out"
[[ -f $tmp/personal-pre-commit ]] || { echo "FAIL chain: the personal pre-commit hook did not run"; fail=1; }
rm -f "$tmp/personal-pre-commit"
setid "Other Person" other.person@example.invalid
out=$(git commit -q --allow-empty -m "feat: bad chained" 2>&1); check "chain: a refused identity still fails" 1 $? "$out"
[[ ! -f $tmp/personal-pre-commit ]] || { echo "FAIL chain: the personal hook ran after a refusal"; fail=1; }
setid procoduck coder@procodus.dev
git checkout -q -b chainside && git commit -q --allow-empty --no-verify -m "feat: side" && git checkout -q main
git commit -q --allow-empty --no-verify -m "feat: main side"
out=$(git merge --no-ff -m "merge: chained" chainside 2>&1); check "chain: a failing personal pre-merge-commit hook blocks the merge" 1 $? "$out"
[[ -f $tmp/personal-pre-merge-commit ]] || { echo "FAIL chain: the personal pre-merge-commit hook did not run"; fail=1; }
git merge --abort 2>/dev/null; git reset -q --hard HEAD
rm -f .git/hooks/pre-commit .git/hooks/pre-merge-commit

# ship.sh runs the identity check without the local escape hatches
if grep -qE 'env -u KOINON_ALLOWED_IDENTITIES -u KOINON_SKIP_IDENTITY_CHECK hack/git-identity-check.sh commit' "$repo/hack/ship.sh"; then
	echo "ok   ship.sh ignores KOINON_ALLOWED_IDENTITIES and KOINON_SKIP_IDENTITY_CHECK"
else echo "FAIL ship.sh still honours an identity override"; fail=1; fi

# the shipped list keeps the project identity and the GitHub lines, and no scratch identity; extra lines
# (a contributor's own, added in their first pull request) are allowed
got=$(grep -vE '^[[:space:]]*(#|$)' "$repo/hack/git-identities.allowed")
shipped_ok=1
for line in 'procoduck <coder@procodus.dev>' 'GitHub <noreply@github.com>' '* <*@users.noreply.github.com>'; do
	grep -qxF -- "$line" <<<"$got" || shipped_ok=0
done
grep -qF 'Some Person' <<<"$got" && shipped_ok=0
if [[ $shipped_ok -eq 1 ]]; then echo "ok   the allowed list keeps the shipped lines"; else echo "FAIL the allowed list lacks a shipped line or holds the scratch identity"; fail=1; fi

[[ $fail -eq 0 ]] && echo "git-identity-check tests passed"
exit $fail
