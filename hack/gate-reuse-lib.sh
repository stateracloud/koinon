#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Verdict reuse of the local gates (#990). A green verdict is stored under the worktree's preflight
# log directory with a fingerprint of the gate's inputs; the next run of the same gate with the same
# fingerprint reports it from the store instead of running it again. Sourced by hack/preflight.sh
# and hack/lane-gates.sh (after hack/paths-lib.sh; run inside the repository root).
#
# The inputs of a gate are what its rule in hack/paths.rules says can affect it (the CI job rule
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

gate_fingerprint() { # <job|gate> <name> <merge-base>
	local kind=$1 name=$2 f names=""
	while IFS= read -r f; do
		[[ -f $f ]] || continue
		case $kind in
		job) paths_job_triggers "$name" "$f" || continue ;;
		tree) ;; # a gate whose rule only selects it, not lists what it reads: the whole tree is its input
		*) paths_match gate "$name" "$f" || continue ;;
		esac
		names+="$f"$'\n'
	done < <(git ls-files --cached --others --exclude-standard | sort -u)
	{
		echo "$kind $name mb=$3"
		# The scripts job also checks the commits (subject, identity): a new commit is a new input.
		[[ $kind == job && $name == scripts ]] && git rev-parse HEAD
		git hash-object hack/preflight.sh hack/lane-gates.sh hack/paths-lib.sh hack/gate-reuse-lib.sh hack/paths.rules hack/versions.env 2>/dev/null
		printf '%s' "$names"
		printf '%s' "$names" | git hash-object --stdin-paths
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
