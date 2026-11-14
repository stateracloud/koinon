#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Verdict reuse of the local gates (#990). A green verdict is stored under the worktree's preflight
# log directory with a fingerprint of the gate's inputs; the next run of the same gate with the same
# fingerprint reports it from the store instead of running it again. Sourced by hack/preflight.sh
# and hack/lane-gates.sh (after hack/paths-lib.sh; run inside the repository root).
#
# A job or gate that runs Go code (and the `tree` kind) uses the tree kind: git's own snapshot of the
# whole working tree. The inputs of any other gate are what its rule in hack/paths.rules says can affect it (the CI job rule
# for the gates that are CI jobs, the `gate` rule for the others), read from the working tree, so
# an uncommitted edit changes the fingerprint exactly like a commit does. Added to that: the merge
# base with the base ref, and the gate scripts of this directory. A fingerprint never stands for
# a failed, incomplete or skipped run: only a pass is stored.
#
# CI never reuses a verdict: it runs every gate (CI=true, which GitHub Actions sets), and
# KOINON_GATE_REUSE=0 turns reuse off by hand (preflight --no-reuse).
#
#   gate_logdir                      the per-worktree log directory (hack/preflight.sh's)
#   gate_reuse_enabled               success when reuse is on
#   gate_fingerprint <job|gate|tree> <name> <merge-base>   print the fingerprint of the inputs
#   gate_reusable <key> <fingerprint>                 success when a green run with it is stored
#   gate_record <key> <fingerprint>                   store a green run

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

gate_logdir() {
	local root rootkey
	root=$(pwd -P)
	rootkey=$(printf '%s' "$root" | { shasum -a 256 2>/dev/null || sha256sum; } | cut -c1-12)
	echo "${TMPDIR:-/tmp}/koinon-preflight/$rootkey"
}

gate_reuse_enabled() {
	[[ -z ${CI:-} && ${KOINON_GATE_REUSE:-1} != 0 ]]
}

# gate_tree: print the id of a git tree that is a snapshot of the working tree (tracked files as on
# disk, untracked files that are not ignored, deletions, modes, symlink targets, renames). Git builds
# it in a COPY of the index (the copy keeps the stat cache, so an unchanged file is not read again),
# so the real index and the working tree are never touched. Nothing here guesses what changed.
gate_tree() {
	local idx tree bits flag
	idx=$(mktemp "${TMPDIR:-/tmp}/gate-index.XXXXXX") || return 1
	# cp -p keeps the index mtime (a fresh one can hide a racily-clean entry). The copy loses the
	# assume-unchanged and skip-worktree bits, or `git add -A` would skip edits to those files; the
	# real index keeps them.
	cp -p "$(git rev-parse --git-path index)" "$idx" 2>/dev/null || : >"$idx"
	# (update-index applies only one of the two flags per call, so it runs once for each.)
	bits=$(GIT_INDEX_FILE=$idx git ls-files -v -z 2>/dev/null | perl -0ne 'print substr($_,2) if /^(?:[a-z]|S) /' | tr '\0' '\1')
	if [[ -n $bits ]]; then
		for flag in --no-assume-unchanged --no-skip-worktree; do
			printf '%s' "$bits" | tr '\1' '\0' | GIT_INDEX_FILE=$idx xargs -0 git update-index "$flag" -- >/dev/null 2>&1 || true
		done
	fi
	if GIT_INDEX_FILE=$idx git add -A -- . >/dev/null 2>&1; then
		tree=$(GIT_INDEX_FILE=$idx git write-tree)
	fi
	rm -f "$idx"
	[[ -n ${tree:-} ]] || return 1
	printf '%s\n' "$tree"
}

gate_fingerprint() { # <job|gate|tree> <name> <merge-base>
	local kind=$1 name=$2 f blob sel="" mfile="" tree
	# RULE (#990, three review rounds of false greens): a job or gate that compiles or runs Go code
	# (go, chartsmoke, harnesses, escape, fuzz, tidy, generate, and any other make job that runs
	# `go test` or `go build`) fingerprints the WHOLE tree. A narrow per-job path list only guesses
	# what the Go build reads (imports, embeds, testdata, go.mod, the computed package graph of
	# hack/ci-changes.sh). Only a non-Go script or lint gate whose inputs are declared in
	# hack/paths.rules and tripwire-tested may keep a narrower fingerprint.
	if [[ $kind == job ]]; then
		case $name in go | chartsmoke | harnesses | escape | fuzz | tidy | generate) kind=tree ;; esac
	fi
	tree=$(gate_tree) || { echo "gate_fingerprint: cannot snapshot the tree" >&2; return 1; }
	if [[ $kind == tree ]]; then
		sel="tree $tree"$'\n'
	else
		mfile=$tree
	fi
	# NUL-separated records ("<mode> <type> <blob>\t<path>"), so a path with quotes or newlines matches the rules as is
	while IFS=$'\t' read -r -d '' blob f; do
		[[ -n $f ]] || continue
		case $kind in
		job) paths_job_triggers "$name" "$f" || paths_match everything - "$f" || paths_match everything ci "$f" || continue ;;
		*) paths_match gate "$name" "$f" || paths_match everything - "$f" || paths_match everything ci "$f" || continue ;;
		esac
		sel+="$f"$'\t'"$blob"$'\n'
	done < <([[ -n $mfile ]] && git ls-tree -r -z "$mfile") # nothing for the tree kind: the tree id stands for every file
	{
		echo "$kind $name mb=$3"
		# The scripts job also checks the commits (subject, identity): a new commit is a new input.
		[[ $2 == scripts ]] && git rev-parse HEAD
		git hash-object hack/preflight.sh hack/lane-gates.sh hack/paths-lib.sh hack/gate-reuse-lib.sh hack/paths.rules hack/versions.env hack/ci-changes.sh 2>/dev/null
		printf '%s' "$sel"
	} | { shasum -a 256 2>/dev/null || sha256sum; } | cut -d' ' -f1
}

gate_reusable() { # <key> <fingerprint>
	gate_reuse_enabled || return 1
	[[ -n $2 && $(cat "$(gate_logdir)/verdicts/$1" 2>/dev/null) == "$2" ]]
}

gate_record() { # <key> <fingerprint>
	gate_reuse_enabled || return 0
	mkdir -p "$(gate_logdir)/verdicts" && printf '%s\n' "$2" >"$(gate_logdir)/verdicts/$1.tmp.$$" &&
		mv "$(gate_logdir)/verdicts/$1.tmp.$$" "$(gate_logdir)/verdicts/$1"
}
