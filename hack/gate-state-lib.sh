#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Where the local gates write their tool state (#1296). A lane runs hack/lane-gates.sh inside the
# Claude Code sandbox, which allows writes only to the worktree, $TMPDIR and the entries of
# .claude/settings.json. The tools under the gates write state outside those paths by default
# (buildx its activity file under ~/.docker/buildx, pnpm its store under ~/.local/share or
# ~/Library, buf its module cache under ~/.cache/buf), so a sandboxed run got EPERM instead of a
# verdict. gate_state_env points each one at a path the sandbox allows. Sourced by hack/lane-gates.sh after hack/gate-reuse-lib.sh (it uses
# gate_logdir), inside the repository root.
#
# A value that is already set wins: CI and a person who exports one keep their own.
#
#   BUILDX_CONFIG       <gate log dir>/state/buildx, a per-worktree copy of the builder selection
#                       (current, instances, defaults, certs) of ${DOCKER_CONFIG:-~/.docker}/buildx. Only
#                       client state lives there; the build cache stays in the builder, so nothing is
#                       rebuilt. The copy is made again each run, so `docker buildx use` still counts.
#   DOCKER_CONFIG       <gate log dir>/state/docker, with a symbolic link to each entry of ~/.docker
#                       (config.json, contexts, cli-plugins, ...) except buildx. BuildKit's registry
#                       auth writes its .token_seed file into this directory on each image pull of a
#                       build. The links keep the credentials, contexts and plugins in one place.
#   KOINON_PNPM_STORE   $TMPDIR/koinon-pnpm-store, the store of `make test-opencode-plugin`. It is
#                       shared by the lanes of one machine. The web gate keeps the default store: its
#                       node_modules were linked from it and it installs offline from it. pnpm
#                       relinks node_modules when the store changes, so the first gate run in a
#                       worktree (and a later plain `make test-opencode-plugin`) installs again from
#                       the registry: slower, not wrong.
#   BUF_CACHE_DIR       $TMPDIR/koinon-buf-cache, the module cache of buf (the proto and generate
#                       gates). buf writes it under ~/.cache/buf by default. The lanes with the same
#                       $TMPDIR share it; buf locks its entries, so a shared cache is safe. The first
#                       run with an empty cache downloads the BSR modules from buf.build.
#
# Not moved here, on purpose:
#   - the envtest port locks (controller-runtime's os.UserCacheDir()/kubebuilder-envtest). They
#     stop two envtest processes from taking the same port, so all lanes of a machine must share
#     them, and on macOS only $HOME moves them. Where the directory does not exist, the package
#     falls back to $TMPDIR/kubebuilder-envtest by itself. Where it exists, the sandbox must allow
#     it: gate_envtest_lock_problem says when it does not, so those gates are notrun, not fail.
#   - the envtest binaries. hack/lane-gates.sh finds them once (setup-envtest only reads a store
#     that has the version) and passes KOINON_ENVTEST_ASSETS to the Makefile.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

gate_state_env() {
	local state src f
	state=$(gate_logdir)/state
	if [[ -z ${BUILDX_CONFIG:-} ]]; then
		src=${DOCKER_CONFIG:-$HOME/.docker}/buildx
		mkdir -p "$state/buildx" || return 1
		for f in current instances defaults certs; do
			# Replace, not merge: a builder removed with `docker buildx rm` must go from the copy too.
			rm -rf "${state:?}/buildx/$f"
			# A copy that fails is not fatal: buildx then uses the default builder of the Docker context,
			# and a gate that needs another builder fails or is notrun on its own.
			if [[ -e $src/$f ]] && ! cp -R "$src/$f" "$state/buildx/$f" 2>/dev/null; then
				echo "lane-gates: warning: cannot copy $src/$f; buildx uses the default builder" >&2
			fi
		done
		export BUILDX_CONFIG=$state/buildx
	fi
	if [[ -z ${DOCKER_CONFIG:-} ]]; then
		# Links, not copies: config.json can hold registry credentials, and a link puts no second copy
		# of them on disk. Made again each run, so an entry removed from ~/.docker goes here too.
		# A gate must not write config.json (docker login, docker context use): the write goes through
		# the link and fails across file systems or in the sandbox. Like the buildx copy, two runs that
		# start at the same time in one worktree can see a half-made directory for a moment.
		src=$HOME/.docker
		rm -rf "${state:?}/docker"
		mkdir -p "$state/docker" || return 1
		if [[ -d $src ]]; then
			for f in "$src"/* "$src"/.[!.]*; do
				[[ -e $f || -L $f ]] || continue
				[[ ${f##*/} == buildx || ${f##*/} == .token_seed* ]] && continue
				ln -s "$f" "$state/docker/${f##*/}" || return 1
			done
		fi
		export DOCKER_CONFIG=$state/docker
	fi
	if [[ -z ${KOINON_PNPM_STORE:-} ]]; then
		export KOINON_PNPM_STORE=${TMPDIR:-/tmp}/koinon-pnpm-store
	fi
	if [[ -z ${BUF_CACHE_DIR:-} ]]; then
		export BUF_CACHE_DIR=${TMPDIR:-/tmp}/koinon-buf-cache
	fi
}

# gate_envtest_lock_dir: the directory of controller-runtime's envtest port locks (os.UserCacheDir).
# LANE_GATES_ENVTEST_LOCKDIR (test hook of hack/lane-gates-test.sh) replaces it.
gate_envtest_lock_dir() {
	if [[ -n ${LANE_GATES_ENVTEST_LOCKDIR:-} ]]; then
		echo "$LANE_GATES_ENVTEST_LOCKDIR"
	elif [[ $(uname -s) == Darwin ]]; then
		echo "$HOME/Library/Caches/kubebuilder-envtest"
	else
		echo "${XDG_CACHE_HOME:-$HOME/.cache}/kubebuilder-envtest"
	fi
}

# gate_envtest_lock_problem: prints a reason when the port lock directory exists but this process
# cannot write in it (a sandbox denial). A real file write, not `test -w`: macOS's sandbox denies
# at open(2), and access(2) does not see it. The port cache reads only port-* files, so the probe
# file never touches a lock.
gate_envtest_lock_problem() {
	local d probe
	d=$(gate_envtest_lock_dir)
	[[ -e $d ]] || return 0
	probe=$d/.koinon-write-probe.$$
	if (: >"$probe") 2>/dev/null; then
		rm -f "$probe"
		return 0
	fi
	echo "sandbox denial: the envtest port locks in $d are not writable; allow that directory in the sandbox"
}
