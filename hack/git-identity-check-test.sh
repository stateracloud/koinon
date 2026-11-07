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

out=$(hack/git-identity-check.sh range "$base" nope 2>&1); check "range: an unknown revision fails closed" 2 $? "$out"
out=$(hack/git-identity-check.sh range 2>&1); check "range: misuse exits 2" 2 $? "$out"
out=$(hack/git-identity-check.sh bogus 2>&1); check "unknown mode exits 2" 2 $? "$out"

# the shipped list is exactly the project identity, GitHub's own and the noreply glob: no personal name
want=$'procoduck <coder@procodus.dev>
GitHub <noreply@github.com>
* <*@users.noreply.github.com>'
got=$(grep -vE '^[[:space:]]*(#|$)' "$repo/hack/git-identities.allowed")
if [[ "$got" == "$want" ]]; then echo "ok   the allowed list is the shipped set only"; else echo "FAIL the allowed list differs from the shipped set"; fail=1; fi

[[ $fail -eq 0 ]] && echo "git-identity-check tests passed"
exit $fail
