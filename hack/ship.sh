#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Ships the current feature branch: commit (signed off, checked identity), blob check, merge
# origin/main, gitleaks, push, pull request, board, ready, auto-merge. One line per step.
#
# Maintainer script (hack/README.md): the board and the CodeRabbit request need the upstream project's
# rights. From a fork the commit, checks, push and pull request work; when the scripts act on the fork
# (a clone without an `upstream` remote, or KOINON_REPO=<fork>) the board step is skipped (no board).
#
#   hack/ship.sh [--message-file F | -m SUBJECT] [--pr-title T --pr-body FILE] [--draft] [--ready]
#                [--e2e] [--auto-merge] [--hold] [--admin] [--no-issue] [--dry-run]
#
# Before the push it looks up the branch's PR. A MERGED or CLOSED one is a refusal (exit 1): a push
# cannot reach main through it; the message says how to continue on a new branch. Pushing to an open
# PR that has auto-merge on prints a warning (it can merge at the old head before CI sees the new
# commits); --hold turns auto-merge off before the push and prints the command that turns it back on.
# --e2e adds the `e2e` label before the push (an open PR) or at PR creation: it forces the merge queue's e2e (the
# label is read when the queue builds the entry, ADR 0105) and runs the advisory quickstart on the PR. Creation
# labels in a second call; e2e-kind's `opened` run reads the labels live (old issue 468).
#
# --ready on a draft PR that exists marks it ready BEFORE the push (old issue 675): ready
# after the push starts a second ci run on the same head. A new PR is created ready, so it has one run.
#
# Run it inside a checkout or worktree on a feature branch named <type>/<N>-<slug> (the claim and the
# PR hang off N); any other name is refused before anything is pushed (old issue 485). Work that needs no issue
# (the claim check only fails on someone else's live claim, so a PR without Closes #N passes, ADR 0094
# decision 2) passes --no-issue, which accepts <type>/<slug> instead. The git identity must be on
# hack/git-identities.allowed (hack/git-identity-check.sh commit, before anything is committed). Environment:
#   KOINON_COMMIT_FOOTER        text appended to the commit message (attribution lines)
#   KOINON_SHIP_MAX_BLOB_MIB    blob size limit in MiB (default 5)
#   KOINON_REPO, KOINON_BOARD, KOINON_BOARD_OWNER  the repository and project board (hack/repo-lib.sh)
#
# origin/main is merged into the branch only when it conflicts (git merge-tree, needs git 2.38; AGENTS.md: merge on a
# conflict, never otherwise). A branch that is behind and merges clean is pushed as it is; the merge queue tests it on
# main's tip. While a merge is in progress with unmerged paths, ship refuses to commit (it would commit the markers).
# --admin is for a pull request that hack/pr-merge.sh --admin will merge (a `.github/**` change, ADR 0105): it skips the
# merge queue, so nothing tests the branch on main's tip, and ship merges origin/main even when it merges clean (D2).
# A failed `gh` read (network, auth) stops ship; "no pull request" is an empty answer and does not.
# --auto-merge hands the pull request to hack/pr-merge.sh (it waits for the checks, then enqueues).
#
# Steps: commit ok <sha> | blobs ok | up to date with origin/main | behind origin/main, no conflict: not merged |
#        merged origin/main | gitleaks ok (N commits) | pushed <branch> | PR #N <url> | board ok|none | ready |
#        auto-merge handed to pr-merge.sh
# Exit: 0 ok, 1 a gate failed (named on stderr), 2 usage, 4 merge conflict (or unmerged paths before a commit).
# The gitleaks gate is never skipped and nothing is ever force-pushed.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=repo-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/repo-lib.sh"
koinon_use_repo # KOINON_REPO picks the repository for every gh call below

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd) # before the cd below: ship may be run by a relative path
msg_file="" subject="" pr_title="" pr_body="" draft=0 ready=0 e2e=0 auto=0 hold=0 dry=0 admin=0 no_issue=0 ready_done=0 cr_fresh=0
need() { [[ $# -ge 2 ]] || { echo "ship: $1 needs a value" >&2; exit 2; }; }
while [[ $# -gt 0 ]]; do
	case $1 in
	--message-file) need "$@"; msg_file=$2; shift 2 ;;
	-m) need "$@"; subject=$2; shift 2 ;;
	--pr-title) need "$@"; pr_title=$2; shift 2 ;;
	--pr-body) need "$@"; pr_body=$2; shift 2 ;;
	--draft) draft=1; shift ;;
	--ready) ready=1; shift ;;
	--e2e) e2e=1; shift ;;
	--auto-merge) auto=1; shift ;;
	--hold) hold=1; shift ;;
	--admin) admin=1; shift ;;
	--review) echo "ship: --review is gone (ADR 0104): a merge releases the claim; after a close without a merge run claim.sh release-pr <PR> or release <N>" >&2; exit 2 ;;
	--no-issue) no_issue=1; shift ;;
	--dry-run) dry=1; shift ;;
	-h | --help) sed -n 4,32p "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) echo "ship: unknown argument $1 (see --help)" >&2; exit 2 ;;
	esac
done
[[ -z $msg_file || -z $subject ]] || { echo "ship: use --message-file or -m, not both" >&2; exit 2; }
[[ -z $msg_file || -r $msg_file ]] || { echo "ship: cannot read $msg_file" >&2; exit 2; }
[[ -z $pr_title && -z $pr_body ]] || [[ -n $pr_title && -n $pr_body ]] || { echo "ship: --pr-title and --pr-body go together" >&2; exit 2; }
[[ -z $pr_body || -r $pr_body ]] || { echo "ship: cannot read $pr_body" >&2; exit 2; }
[[ $draft == 0 || $ready == 0 ]] || { echo "ship: --draft and --ready contradict" >&2; exit 2; }
[[ $auto == 0 || $draft == 0 ]] || { echo "ship: --auto-merge and --draft contradict (pr-merge.sh refuses a draft)" >&2; exit 2; }
[[ $draft == 0 || -n $pr_title ]] || { echo "ship: --draft needs --pr-title/--pr-body" >&2; exit 2; }

fail() { echo "ship: FAIL $*" >&2; exit 1; }
say() { echo "$*"; }
# gh_ask <what> <gh args...>: the answer of a gh read. A failed call stops ship with gh's own error; an empty answer is
# a real answer (no pull request), so it passes. Without this a broken token looked like "no pull request".
gh_ask() {
	local what=$1 out err rc=0
	shift
	err=$(mktemp "${TMPDIR:-/tmp}/ship.XXXXXX")
	out=$(gh "$@" 2>"$err") || rc=$?
	if [[ $rc != 0 ]]; then
		local msg
		msg=$(head -c 300 "$err" | tr '\n' ' ')
		rm -f "$err"
		fail "could not ask gh for $what (exit $rc): ${msg:-no error text}"
	fi
	rm -f "$err"
	printf '%s\n' "$out"
}

cd "$(git rev-parse --show-toplevel)"
branch=$(git symbolic-ref --short -q HEAD) || fail "detached HEAD; check out a feature branch"
[[ $branch != main && $branch != master ]] || fail "refusing to ship from $branch; use a feature branch"
# The board and the upstream come from the checkout (hack/repo-lib.sh); a bad value fails here, before the push.
board=$(koinon_board) || fail "cannot work out the project board (hack/repo-lib.sh)"
up=$(koinon_upstream) || fail "cannot read the upstream repository (hack/repo-lib.sh)"
# A branch is <type>/<N>-<slug> (the claim and the PR hang off N), or <type>/<slug> with --no-issue for
# work without an issue. A stray name, such as the one a broken test helper once checked out in
# a real worktree (old issue 485), is refused before anything is pushed.
if [[ $no_issue == 1 ]]; then
	[[ $branch =~ ^[a-z]+/[a-z0-9][a-z0-9._-]{2,}$ ]] || fail "branch '$branch' is not <type>/<slug>; rename it (git branch -m) before shipping"
else
	[[ $branch =~ ^[a-z]+/[0-9]+-[a-z0-9][a-z0-9._-]*$ ]] || fail "branch '$branch' is not <type>/<N>-<slug>; rename it (git branch -m), or pass --no-issue for work without an issue"
fi

# Identity: commits and the merge commit use git config; refuse one that is not on the allowlist.
name=$(git config user.name || true)
email=$(git config user.email || true)
[[ -n $name && -n $email ]] || fail "git user.name/user.email not set"
# The same check the commit hooks and CI run (koinon#320): author and committer must be on the allowlist.
[[ -x hack/git-identity-check.sh ]] || fail "hack/git-identity-check.sh is missing: this branch was cut before koinon#320; run 'git merge origin/main' (never rebase), then ship again"
# Ship ignores the local escape hatches (KOINON_ALLOWED_IDENTITIES, KOINON_SKIP_IDENTITY_CHECK): CI would refuse the commits anyway.
id_out=$(env -u KOINON_ALLOWED_IDENTITIES -u KOINON_SKIP_IDENTITY_CHECK hack/git-identity-check.sh commit 2>&1) || { echo "$id_out" >&2; fail "identity: $name <$email> is not allowed (hack/git-identities.allowed)"; }
say "identity $email"

limit_mib=${KOINON_SHIP_MAX_BLOB_MIB:-5}
limit=$((limit_mib * 1024 * 1024))

# A merge or cherry-pick that stopped on a conflict leaves unmerged paths. `git add -A` would stage the markers and the
# commit below would record them, so a re-run of ship after a conflict (exit 4) must stop here until they are resolved.
unmerged=$(git diff --name-only --diff-filter=U)
if [[ -n $unmerged ]]; then
	echo "ship: unmerged paths (resolve them, git add them, commit; then run ship again):" >&2
	sed 's/^/ship:   /' <<<"$unmerged" >&2
	[[ ! -e $(git rev-parse --git-path MERGE_HEAD) ]] || echo "ship: a merge is in progress (MERGE_HEAD exists)" >&2
	exit 4
fi

dirty=0
[[ -n $(git status --porcelain) ]] && dirty=1

# 1. commit
if [[ $dirty == 1 ]]; then
	[[ -n $msg_file || -n $subject ]] || { echo "ship: uncommitted changes and no --message-file/-m" >&2; exit 2; }
	if [[ $dry == 1 ]]; then
		say "dry-run: would commit all changes with --signoff as $name <$email>"
	else
		git add -A
		# Catch a huge file before it is committed.
		while IFS= read -r -d '' f; do
			size=$(git cat-file -s ":$f" 2>/dev/null || echo 0)
			[[ $size -le $limit ]] || { git reset -q; fail "blobs: $f is $((size / 1024 / 1024)) MiB, over the $limit_mib MiB limit (not committed)"; }
		done < <(git diff --cached --name-only --diff-filter=AM -z)
		tmp=$(mktemp "${TMPDIR:-/tmp}/ship.XXXXXX")
		trap 'rm -f "$tmp"' EXIT
		if [[ -n $msg_file ]]; then cat "$msg_file" >"$tmp"; else printf '%s\n' "$subject" >"$tmp"; fi
		if [[ -n ${KOINON_COMMIT_FOOTER:-} ]]; then printf '\n%s\n' "$KOINON_COMMIT_FOOTER" >>"$tmp"; fi
		git commit -q --signoff -F "$tmp"
		say "commit ok $(git rev-parse --short HEAD)"
	fi
else
	say "commit skip (nothing to commit)"
fi

# 3. blobs: any blob added in origin/main..HEAD over the limit
# The gate cannot say "ok" about a range it cannot read: a base that does not resolve fails it.
git rev-parse --verify -q origin/main >/dev/null || fail "blobs: origin/main does not resolve (git fetch origin main); the blob check has no base"
objs=$(git rev-list --objects origin/main..HEAD) || fail "blobs: git rev-list origin/main..HEAD failed"
big=$(printf '%s' "$objs" | git cat-file --batch-check='%(objecttype) %(objectsize) %(rest)' |
	awk -v l="$limit" '$1 == "blob" && $2 > l && n++ < 3 { printf "%s (%d MiB)\n", $3, $2 / 1048576 }') || fail "blobs: git cat-file failed"
[[ -z $big ]] || fail "blobs: over the $limit_mib MiB limit: $(tr '\n' ' ' <<<"$big")"
say "blobs ok"

# 4. fetch and merge origin/main
if [[ $dry == 1 ]]; then
	[[ $e2e == 1 ]] && say "dry-run: would add the e2e label before the push"
	say "dry-run: would git fetch origin, merge origin/main only if it conflicts, run gitleaks, push $branch"
else
	git fetch -q origin
	# Merge only on a conflict (D2): ask git merge-tree first. Exit 0 clean, 1 conflict, anything else is an error.
	mt_rc=0
	if git merge-base --is-ancestor origin/main HEAD; then
		say "up to date with origin/main"
	else
		git merge-tree --write-tree origin/main HEAD >/dev/null 2>&1 || mt_rc=$?
		[[ $mt_rc == 0 || $mt_rc == 1 ]] || fail "git merge-tree could not check origin/main for conflicts (exit $mt_rc; it needs git 2.38 or newer)"
		if [[ $mt_rc == 0 && $admin == 1 ]]; then mt_rc=1; else [[ $mt_rc == 1 ]] || say "behind origin/main, no conflict: not merged"; fi
	fi
	if [[ $mt_rc == 0 ]]; then
		:
	elif merge_out=$(git merge -q --no-edit --signoff origin/main 2>&1); then
		say "merged origin/main"
	else
		conflicts=$(git diff --name-only --diff-filter=U)
		if [[ -z $conflicts ]]; then
			# no unmerged files: a hook (pre-merge-commit identity check) refused, not a conflict
			git merge --abort >/dev/null 2>&1 || true
			echo "ship: merge of origin/main failed without conflicts (a git hook refused?):" >&2
			echo "$merge_out" >&2
			exit 1
		elif [[ $conflicts == docs/adr/README.md ]]; then
			hack/adr-index.sh >/dev/null
			git add docs/adr/README.md
			git commit -q --no-edit --signoff
			say "merged origin/main (regenerated docs/adr/README.md)"
		else
			echo "ship: merge conflict in: $(tr '\n' ' ' <<<"$conflicts")" >&2
			echo "ship: resolve them, git add them, commit, and run ship again (merge in progress; ship refuses to commit while paths are unmerged)" >&2
			exit 4
		fi
	fi

	# 4b. the branch's PR: a merged or closed one cannot take this push (old issue 431)
	prinfo=$(gh_ask "the pull request of $branch" pr list --head "$branch" --state all --json number,state,autoMergeRequest --jq \
		'(map(select(.state == "OPEN"))[0] // (sort_by(.number) | last)) // empty | [.state, (.number | tostring), (.autoMergeRequest != null | tostring)] | @tsv')
	pr_state="" pr_num="" pr_auto=false
	if [[ -n $prinfo ]]; then IFS=$'\t' read -r pr_state pr_num pr_auto <<<"$prinfo"; fi
	case $pr_state in
	MERGED | CLOSED)
		echo "ship: PR #$pr_num for $branch is $pr_state; a push to $branch cannot reach main (nothing pushed)" >&2
		echo "ship: continue on a new branch from the unmerged commits:" >&2
		echo "ship:   hack/wt.sh new <name> --from $branch --branch <new-branch>" >&2
		exit 1
		;;
	OPEN)
		if [[ $pr_auto == true && $hold == 1 ]]; then
			gh pr merge "$pr_num" --disable-auto >/dev/null || fail "could not turn auto-merge off on PR #$pr_num"
			say "hold: auto-merge off on PR #$pr_num"
			say "turn it back on after CI is green: hack/pr-merge.sh $pr_num"
		elif [[ $pr_auto == true ]]; then
			echo "ship: warning: PR #$pr_num has auto-merge on; it can merge at the old head before CI sees the new commits (use --hold to turn it off for the push)" >&2
		fi
		;;
	esac

	# 5. gitleaks over every commit CI will scan
	command -v gitleaks >/dev/null || fail "gitleaks is not installed"
	n=$(git rev-list --count origin/main..HEAD)
	glog=$(mktemp "${TMPDIR:-/tmp}/ship.XXXXXX")
	if ! gitleaks git --redact --no-banner --log-opts="origin/main..HEAD" . >"$glog" 2>&1; then
		tail -n 8 "$glog" >&2
		rm -f "$glog"
		fail "gitleaks found a leak in origin/main..HEAD (nothing pushed)"
	fi
	rm -f "$glog"
	say "gitleaks ok ($n commits)"

	# 6. push. The e2e label goes on an open PR first, so the push already sees it; a new PR gets it
	# at creation (step 7).
	if [[ $e2e == 1 ]]; then
		label_pr=$(gh_ask "the open pull request of $branch" pr list --head "$branch" --state open --json url --jq '.[0].url // empty')
		if [[ -n $label_pr ]]; then
			gh pr edit "$label_pr" --add-label e2e >/dev/null || fail "could not add the e2e label"
			say "label e2e"
		fi
	fi
	# Ready BEFORE the push (old issue 675): a draft PR marked ready after the push runs the ci workflow twice on
	# one head (the synchronize run, then the ready_for_review run), seconds apart. Marked ready first,
	# the ready_for_review run starts on the old head and the push's run supersedes it (an older head is
	# cancelled, ci.yml).
	if [[ $ready == 1 && $pr_state == OPEN ]]; then
		if [[ $(gh pr view "$pr_num" --json isDraft --jq .isDraft 2>/dev/null || echo false) == true ]]; then
			gh pr ready "$pr_num" >/dev/null || fail "gh pr ready failed"
			ready_done=1
			say "ready"
		fi
	fi
	old_head=$(git rev-parse -q --verify "origin/$branch" 2>/dev/null || true)
	# git prints the server's "remote:" banner on stderr even with -q; keep it out of the step lines.
	if ! pushlog=$(git push -q -u origin "$branch" 2>&1); then
		printf '%s\n' "$pushlog" >&2
		if [[ $ready_done == 1 ]]; then
			# The PR was marked ready before the push; without the push it would sit ready at the old head.
			if gh pr ready --undo "$pr_num" >/dev/null 2>&1; then
				fail "push failed (the pull request was put back to draft with gh pr ready --undo)"
			fi
			fail "push failed, and gh pr ready --undo $pr_num failed too: the pull request is still ready at the old head; run it by hand"
		fi
		fail "push failed"
	fi
	say "pushed $branch"
	[[ $(git rev-parse HEAD) == "$old_head" ]] || cr_fresh=1
fi

# 7. pull request
pr_url=""
if [[ -n $pr_title || $ready == 1 || $auto == 1 ]]; then
	if [[ $dry == 1 ]]; then
		say "dry-run: would create or update the PR${pr_title:+ \"$pr_title\"}$([[ $draft == 1 ]] && echo ' (draft)'), add it to the board"
		[[ $ready == 1 ]] && say "dry-run: would mark ready, then post @coderabbitai review once per head ($up only)"
		[[ $auto == 1 ]] && say "dry-run: would hand the pull request to hack/pr-merge.sh"
	else
		pr_url=$(gh_ask "the open pull request of $branch" pr list --head "$branch" --state open --json url --jq '.[0].url // empty')
		if [[ -n $pr_title ]]; then
			if [[ -n $pr_url ]]; then
				gh pr edit "$pr_url" --title "$pr_title" --body-file "$pr_body" >/dev/null
			else
				args=(--head "$branch" --title "$pr_title" --body-file "$pr_body")
				[[ $draft == 1 ]] && args+=(--draft)
				[[ $e2e == 1 ]] && args+=(--label e2e)
				pr_url=$(gh pr create "${args[@]}" | tail -n 1)
			fi
		fi
		[[ -n $pr_url ]] || fail "no open pull request for $branch (pass --pr-title and --pr-body)"
		say "PR #${pr_url##*/} $pr_url"
		if [[ -z $board ]]; then
			say "board none"
		elif gh project item-add "${board#* }" --owner "${board%% *}" --url "$pr_url" >/dev/null 2>&1; then
			say "board ok"
		else
			echo "ship: warning: could not add $pr_url to the board" >&2
			say "board warn"
		fi
		if [[ $ready == 1 && $ready_done != 1 ]]; then
			gh pr ready "$pr_url" >/dev/null || fail "gh pr ready failed"
			say "ready"
		fi
		# CodeRabbit (#184) reviews manually here: ask once per head, after the push. Advisory: a failure only warns.
		if [[ $ready == 1 && ${pr_url,,} == */"${up,,}"/pull/* ]]; then
			# A head this run pushed is new: ask without the date check (GitHub moves the PR head after the push, and a
			# request on the old head says nothing about this one). Otherwise a request dated after the head commit counts.
			if [[ $cr_fresh == 1 ]]; then
				cr_n=0
			else
				# shellcheck disable=SC2016 # $o and $h are jq variables
				cr_n=$(gh pr view "$pr_url" --json headRefOid,comments,commits --jq '.headRefOid as $o | ([.commits[] | select(.oid == $o) | .committedDate][0] // "") as $h | [.comments[] | select((.body | rtrimstr("\n")) == "@coderabbitai review") | select(.createdAt > $h)] | length' 2>/dev/null || echo err)
			fi
			if [[ $cr_n == 0 ]]; then
				if gh pr comment "$pr_url" --body "@coderabbitai review" >/dev/null 2>&1; then
					say "coderabbit review requested"
				else
					echo "ship: warning: could not post @coderabbitai review on $pr_url" >&2
					say "coderabbit warn"
				fi
			elif [[ $cr_n =~ ^[0-9]+$ ]]; then
				say "coderabbit review already requested on this head"
			else
				echo "ship: warning: could not read the comments of $pr_url; no @coderabbitai review posted" >&2
				say "coderabbit warn"
			fi
		fi
		if [[ $auto == 1 ]]; then
			# The push moves the head on GitHub a few seconds later; pr-merge.sh pins the head it reads, so wait for it.
			want_head=$(git rev-parse HEAD)
			for ((hw = 0; hw <= ${SHIP_HEAD_WAIT:-120}; hw += 5)); do
				[[ $(gh_ask "the head of $pr_url" pr view "$pr_url" --json headRefOid --jq .headRefOid) != "$want_head" ]] || break
				[[ $hw -lt ${SHIP_HEAD_WAIT:-120} ]] || fail "GitHub still shows another head than ${want_head:0:7} on $pr_url after ${SHIP_HEAD_WAIT:-120}s; run hack/pr-merge.sh ${pr_url##*/} later"
				sleep 5
			done
			# pr-merge.sh reads the head and every required check, then enqueues (ADR 0105: the merge queue runs the kind
			# e2e on the queue commit when the change needs it). SHIP_PR_MERGE is a test hook.
			pm_rc=0
			"${SHIP_PR_MERGE:-$script_dir/pr-merge.sh}" "${pr_url##*/}" || pm_rc=$?
			[[ $pm_rc == 0 ]] || fail "hack/pr-merge.sh ${pr_url##*/} exited $pm_rc (the pull request stays open; fix that, then run it again)"
			say "auto-merge handed to pr-merge.sh"
		fi
	fi
fi
