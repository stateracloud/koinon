#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs the repo gates that your change can affect, before you push. The gates are the ones CI and
# AGENTS.md name; this picks them from the changed files so nobody runs them by hand one by one.
#
#   hack/preflight.sh [--base origin/main] [--only g1,g2] [--list] [--no-reuse] [-v]
#   hack/preflight.sh --stop
#
#   --base REF    compare against REF (default origin/main); the changed files are
#                 `git diff --name-only REF...HEAD` plus uncommitted and untracked files
#   --only LIST   run exactly these gates (comma list), whether or not the change triggers them
#   --list        print which gates would run and why, then exit
#   -v            print each gate's full output
#   --no-reuse    run every selected gate even when a green verdict for the same inputs is stored
#                 (hack/gate-reuse-lib.sh; CI=true or KOINON_GATE_REUSE=0 do the same)
#   --stop        stop every run of this worktree (one pid file per gate in its log directory) and
#                 exit; never touches another worktree's run. Do not pkill/pgrep by script name instead.
# One pid file per gate (`preflight.<gate>.pid`): two runs of different gates in one worktree do not
# block each other (hack/lane-gates.sh runs its gates in parallel); two runs of one gate do.
#
# Gates: go tidy vuln fuzz deps generate proto migrate adr chart web shell actionlint zizmor runnerlint dockerfile mktemp pinned bash5 audit versions scripts e2e gitleaks.
# Which paths turn a gate on: hack/paths.rules (the same file CI reads). The `scripts` gate runs the
# static checks and script tests of the CI jobs quick checks, lint-shell and scripts: only the lines
# whose `inputs=` a changed file hits (and the `guard` lines), four at a time (KOINON_SCRIPTS_JOBS).
# A gate whose inputs are unchanged since its last green run is reported `ok 0s (reused)`.
# One line per gate: `go ok 12s`, `go FAIL 3s log=<path>` followed by at most 5 lines of the
# error, or `go INCOMPLETE 3s (skipped: docker) log=<path>` when hack/go-test.sh exited 3 (an
# expected requirement was missing, so some suites or specs skipped; old issue 507). Full logs are under
# ${TMPDIR:-/tmp}/koinon-preflight/<hash of the repo root>/, one directory per worktree, so
# parallel lanes never mix logs. The e2e gate only reports the verdict
# of hack/needs-e2e.sh and never fails. Gates never touch a cluster (KUBECONFIG=/dev/null).
# Exit 0 when every gate that ran passed, 1 when one failed, 3 when a gate was incomplete and none
# failed, 2 on a usage error.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

# shellcheck source=/dev/null
. "$(dirname "$0")/paths-lib.sh" || exit 2
# shellcheck source=/dev/null
. "$(dirname "$0")/gate-reuse-lib.sh" || exit 2
paths_load || exit 2 # the path rules (hack/paths.rules) are shared with CI; no private copy here
ALL="go tidy vuln fuzz deps generate proto migrate adr chart web shell actionlint zizmor runnerlint dockerfile mktemp pinned bash5 audit versions scripts e2e gitleaks"
# shellcheck source=/dev/null
. "$(dirname "$0")/versions.env" # GOFUMPT_VERSION, dated by hack/dep-age-check.sh (ADR 0079)
base=origin/main only="" list=0 verbose=0 stop=0 noreuse=0
while [[ $# -gt 0 ]]; do
	case $1 in
	--base) [[ $# -ge 2 ]] || { echo "preflight: --base needs a ref" >&2; exit 2; }; base=$2; shift 2 ;;
	--only) [[ $# -ge 2 ]] || { echo "preflight: --only needs a list" >&2; exit 2; }; only=$2; shift 2 ;;
	--list) list=1; shift ;;
	-v) verbose=1; shift ;;
	--no-reuse) noreuse=1; shift ;;
	--stop) stop=1; shift ;;
	-h | --help) sed -n 4,32p "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) echo "preflight: unknown argument $1 (see --help)" >&2; exit 2 ;;
	esac
done

cd "$(git rev-parse --show-toplevel)" || exit 2
# One log directory and pid file per worktree: a hash of the repo root keys it.
root=$(pwd -P)
logdir=$(gate_logdir)
[[ $noreuse == 1 ]] && export KOINON_GATE_REUSE=0

kill_tree() { # <pid>: SIGTERM the process and its descendants (found by parent pid, never by name)
	local c
	for c in $(pgrep -P "$1" 2>/dev/null); do kill_tree "$c"; done
	kill "$1" 2>/dev/null || true
}

if [[ $stop == 1 ]]; then
	stopped="" pf="" pid=""
	for pf in "$logdir"/preflight.*.pid "$logdir/preflight.pid"; do # the last name: the pid file of an older preflight
		[[ -f $pf ]] || continue
		pid=$(cat "$pf" 2>/dev/null || true)
		if [[ -n $pid ]] && kill -0 "$pid" 2>/dev/null; then
			case " $stopped " in *" $pid "*) ;; *) kill_tree "$pid"; stopped="$stopped${stopped:+ }$pid" ;; esac
		fi
		rm -f "$pf"
	done
	if [[ -n $stopped ]]; then
		echo "preflight: stopped run $stopped of $root"
	else
		echo "preflight: no run in progress for $root"
	fi
	exit 0
fi

git rev-parse --verify --quiet "$base^{commit}" >/dev/null || { echo "preflight: unknown base $base (git fetch origin?)" >&2; exit 2; }

if [[ -n $only ]]; then
	for g in ${only//,/ }; do
		case " $ALL " in *" $g "*) ;; *) echo "preflight: unknown gate $g (gates: $ALL)" >&2; exit 2 ;; esac
	done
fi

export KUBECONFIG=/dev/null

FILES=$({
	git diff --name-only --no-renames "$base...HEAD"
	git diff --name-only --no-renames HEAD
	git ls-files --others --exclude-standard
} 2>/dev/null | sort -u | sed '/^$/d')

match() { grep -Eq -- "$1" <<<"$FILES"; }
count() { grep -Ec -- "$1" <<<"$FILES" || true; }
existing() { # <regexp>: changed files matching the regexp that still exist
	local f
	while IFS= read -r f; do [[ -e $f ]] && echo "$f"; done < <(grep -E -- "$1" <<<"$FILES" || true)
}

# triggering <gate>: the changed files that turn the gate on. generate, chart, web, tidy, vuln, fuzz and scripts are CI
# jobs: the job's rule (hack/paths.rules) decides, so preflight and CI cannot disagree.
triggering() {
	local f
	while IFS= read -r f; do
		[[ -n $f ]] || continue
		case $1 in
		generate | chart | web | tidy | vuln | fuzz | scripts) paths_job_triggers "$1" "$f" && echo "$f" ;;
		*) paths_match gate "$1" "$f" && echo "$f" ;;
		esac
	done <<<"$FILES"
	return 0
}

# reason <gate>: why the change triggers the gate; empty when it does not.
reason() {
	local n
	case $1 in
	e2e) echo "informational, always"; return 0 ;;
	gitleaks) echo "always"; return 0 ;;
	esac
	n=$(triggering "$1" | grep -c . || true)
	[[ $n -gt 0 ]] || return 0
	case $1 in
	go) echo "$n Go files changed" ;;
	tidy) echo "Go files or go.mod/go.sum changed" ;;
	vuln) echo "go.mod or go.sum changed" ;;
	fuzz) echo "a fuzzed package or the fuzz script changed" ;;
	deps) echo "dependency manifests, lockfiles or Dockerfiles changed" ;;
	proto) echo "proto files changed" ;;
	generate) echo "a generator input or output changed" ;;
	migrate) echo "migrations changed" ;;
	adr) echo "docs/adr or the ADR scripts changed" ;;
	chart) echo "charts changed" ;;
	web) echo "web/ changed" ;;
	shell | bash5) echo "$n shell scripts changed" ;;
	actionlint) echo "workflows or the actionlint config changed" ;;
	zizmor) echo "workflows, composite actions or the zizmor config changed" ;;
	runnerlint) echo "workflows, the actionlint config or the runner lint changed" ;;
	dockerfile) echo "a Dockerfile or the Dockerfile lint changed" ;;
	pinned) echo "shell scripts, workflows or composite actions changed" ;;
	mktemp) echo "shell scripts, the Makefile, workflows or Go files under internal/ changed" ;;
	audit) echo "Go code, sqlc queries or the audit batch lint changed" ;;
	versions) echo "the versions file or a copy of one changed" ;;
	scripts) echo "tooling the CI script checks cover changed" ;;
	esac
	return 0
}

selected() { # <gate>: prints the reason when the gate runs, nothing when it does not
	local r
	if [[ -n $only ]]; then
		case ",$only," in *",$1,"*) r=$(reason "$1"); echo "${r:-forced by --only}" ;; esac
	else
		reason "$1"
	fi
}

if [[ $list == 1 ]]; then
	for g in $ALL; do
		r=$(selected "$g")
		[[ -n $r ]] && echo "$g: $r"
	done
	exit 0
fi

# --- gates: each runs in a subshell with `set -e`; its output goes to the gate's log ---------------

# A directory below a nested go.mod (hack/spike/*) is not a package of the root module: golangci-lint
# on ./hack/spike/x fails with "main module does not contain package".
in_nested_module() {
	local d=$1
	while [[ $d != . && $d != "" ]]; do
		[[ -f $d/go.mod ]] && return 0
		d=${d%/*}
		[[ $d == . || $d == "${d%/*}" ]] && break
	done
	return 1
}

go_pkgs() { # changed Go packages as ./dir args; every package (minus test/harnesses) when no Go file changed;
	# nothing when every changed Go file sits in a nested module
	local f d out="" seen=0
	for f in $(existing '\.go$'); do
		seen=1
		d=${f%/*}
		[[ $d == "$f" ]] && d=.
		[[ $d == . ]] || d=./$d
		in_nested_module "$d" && continue
		case " $out " in *" $d "*) ;; *) out="$out $d" ;; esac
	done
	if [[ -z $out && $seen == 1 ]]; then
		return 0
	elif [[ -z $out ]]; then
		# Directories, not import paths: golangci-lint reads each argument as a path under the cwd.
		# `./...` skips nested modules; {{.Dir}} is absolute, so strip the repo root.
		local root=$PWD
		go list -f '{{.Dir}}' ./... | while IFS= read -r d; do
			d=${d#"$root"}
			d=.${d}
			[[ $d == ./test/harnesses ]] || printf '%s ' "$d"
		done
	else
		echo "$out"
	fi
}

g_go() {
	local pkgs files out
	pkgs=$(go_pkgs)
	if [[ -z ${pkgs// /} ]]; then
		echo "only nested-module Go files changed; nothing to lint in the root module"
		return 0
	fi
	if command -v golangci-lint >/dev/null; then
		# `|| return 1`: golangci-lint exits 3 on findings, which run_gate would read as INCOMPLETE;
		# only go-test.sh may report 3. The wrapper gives this worktree its own lock and cache (#236).
		# shellcheck disable=SC2086 # pkgs is a word list
		hack/golangci-lint.sh run $pkgs || return 1
	else
		files=$(existing '\.go$')
		if [[ -n $files ]]; then
			# shellcheck disable=SC2086
			out=$(go run mvdan.cc/gofumpt@$GOFUMPT_VERSION -l $files)
			if [[ -n $out ]]; then echo "gofumpt would change:"; echo "$out"; return 1; fi
		fi
		# shellcheck disable=SC2086
		go vet $pkgs || return 1
	fi
	# hack/go-test.sh, not bare go test: its verdict reports a suite that skipped for a missing
	# requirement (no Docker in a sandbox) as SKIPPED and one that did not run as a failure (old issue 507).
	local reports rc=0
	reports=$(mktemp -d "${TMPDIR:-/tmp}/preflight-go.XXXXXX") || return 1
	# shellcheck disable=SC2086
	REPORT_DIR=$reports hack/go-test.sh $pkgs || rc=$?
	rm -rf "$reports"
	return "$rc"
}

g_tidy() {
	local tmp rc=0
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/preflight-tidy.XXXXXX") || return 1
	cp go.mod "$tmp/go.mod"
	[[ -f go.sum ]] && cp go.sum "$tmp/go.sum"
	go mod tidy || rc=$?
	cmp -s go.mod "$tmp/go.mod" || { echo "go mod tidy changed go.mod"; rc=1; }
	if [[ -f $tmp/go.sum ]]; then cmp -s go.sum "$tmp/go.sum" || { echo "go mod tidy changed go.sum"; rc=1; }; fi
	cp "$tmp/go.mod" go.mod
	[[ -f $tmp/go.sum ]] && cp "$tmp/go.sum" go.sum
	rm -rf "$tmp"
	return $rc
}

g_deps() { hack/dep-age-check.sh "$base"; }

g_proto() {
	buf lint
	# Same against-ref as CI (#542): the merge base of HEAD and the base, not the base tip.
	against=$(hack/buf-breaking-against.sh "$base")
	buf breaking --against ".git#ref=$against"
}

tree_state() { git status --porcelain; git diff | cksum; }
g_generate() {
	local before after
	before=$(tree_state)
	make generate
	after=$(tree_state)
	if [[ $before != "$after" ]]; then
		echo "make generate changed the tree; commit the generated output:"
		git status --porcelain
		return 1
	fi
}

g_migrate() { hack/migrate-lint.sh "$base"; }
g_adr() { hack/adr-lint-test.sh >/dev/null && hack/adr-lint.sh && hack/doc-meta-lint-test.sh >/dev/null && hack/doc-meta-lint.sh; }
g_chart() { hack/chart-check.sh; }
g_web() { (cd web && pnpm lint && pnpm typecheck && pnpm test); }
g_shell() {
	local files
	files=$(existing '\.sh$' || true)
	[[ -n $files ]] || { echo "no existing shell files"; return 0; }
	# -x: CI runs the whole tree, where a sourced file (hack/repo-lib.sh) is itself an input, so SC1091 never fires
	# there; on the changed files alone it would, unless shellcheck follows the `source=` directive (#968).
	# shellcheck disable=SC2086
	shellcheck -x $files
}
g_actionlint() { # the whole tree, as CI does: a workflow's lint can depend on its siblings
	actionlint
}
g_vuln() { # govulncheck at the pinned version, as the go-vuln job does (needs the network: proxy.golang.org, vuln.go.dev)
	hack/govulncheck-test.sh
	hack/govulncheck.sh
}
g_fuzz() { # the time-boxed fuzz targets, as the fuzz job of ci.yml does (FUZZTIME per target, default 20s)
	hack/go-fuzz.sh
}
g_zizmor() { # workflow security lint, offline and default persona as in CI (.github/zizmor.yml)
	zizmor --offline --no-progress .github
}
g_runnerlint() { # GitHub-hosted runners only (#58); needs no actionlint, so it runs where that is missing
	hack/runner-lint-test.sh
	hack/runner-lint.sh
}
g_dockerfile() { # hadolint (pinned in hack/versions.env) and the digest and secret rules; exit 3 when hadolint is missing (#175)
	hack/dockerfile-lint-test.sh
	hack/dockerfile-lint.sh
}
g_mktemp() { # independent of shellcheck, so it runs where shellcheck is missing (old issue 535)
	hack/mktemp-lint-test.sh
	hack/mktemp-lint.sh
}
g_pinned() { # pinned release downloads retry and check sha256 (#431)
	hack/fetch-pinned-test.sh
	hack/pinned-download-lint.sh
}
g_bash5() { # every bash script starts with the bash 5 guard (ADR 0090, old issue 553)
	hack/bash5-lint-test.sh
	hack/bash5-lint.sh
}
g_audit() { # only internal/audit writes the audit tables (the audit batch, #371)
	hack/audit-batch-lint-test.sh
	hack/audit-batch-lint.sh
}
g_versions() { # hack/versions.env against its literal copies (old issue 544)
	hack/versions-check-test.sh
	hack/versions-check.sh
}
# scripts_one <index> <command>: one script-check line, run by xargs -P (see g_scripts). Output goes to the
# results directory, never to the shared log, so parallel lines do not interleave.
scripts_one() {
	local i=$1 cmd=$2 rc=0
	# shellcheck disable=SC2086 # the rule's words are the command and its arguments
	$cmd >"$SCRIPTS_RES/$i.out" 2>&1 || rc=$?
	echo "$rc" >"$SCRIPTS_RES/$i.rc"
	return 0
}
export -f scripts_one

# script_line_fp <inputs> <resolved command>: the fingerprint of one script-check line (hack/gate-reuse-lib.sh):
# its command, its inputs' content, and HEAD for a line that reads the commit range. A guard line has the
# fingerprint of the whole `scripts` job, so it reruns when anything the job reads changed.
script_line_fp() {
	local inp=$1 cmd=$2 f names=""
	if [[ $inp == guard || $inp == - ]]; then
		echo "guard $SCRIPTS_FP"
		return
	fi
	while IFS= read -r f; do
		[[ -f $f ]] && names+="$f"$'\n'
	done < <(for f in ${inp//,/ }; do if [[ $f == */ ]]; then git ls-files --cached --others --exclude-standard -- "$f"; else echo "$f"; fi; done | sort -u)
	{
		echo "cmd=$cmd inputs=$inp"
		[[ $cmd == *"$mb"* ]] && git rev-parse HEAD
		printf '%s' "$names"
		printf '%s' "$names" | git hash-object --stdin-paths
	} | { shasum -a 256 2>/dev/null || sha256sum; } | cut -d' ' -f1
}

g_scripts() { # the CI script checks of hack/paths.rules, selected by their `inputs=` and run KOINON_SCRIPTS_JOBS (4) at a time
	local inp line cmd mb failed=0 name i=0 n=0 skipped=0 reused=0 fp key hit f
	local -a run=() run_fp=() run_key=()
	mb=$(git merge-base "$base" HEAD) || return 1
	SCRIPTS_FP=$(gate_fingerprint job scripts "$mb")
	SCRIPTS_RES=$(mktemp -d "${TMPDIR:-/tmp}/preflight-scripts.XXXXXX") || return 1
	export SCRIPTS_RES
	while IFS=$'\t' read -r inp line; do
		[[ -n $line ]] || continue
		n=$((n + 1))
		cmd=${line//@BASE@/$mb}
		cmd=${cmd//@HEAD@/HEAD}
		# Selected when a changed file is one of the line's inputs; a guard line (and, failing closed, a line
		# with no annotation, which hack/paths-test.sh rejects) always runs.
		hit=0
		if [[ $inp == guard || $inp == - || -z $inp ]]; then
			hit=1
		else
			while IFS= read -r f; do
				[[ -n $f ]] && paths_inputs_hit "$inp" "$f" && { hit=1; break; }
			done <<<"$FILES"
		fi
		if [[ $hit == 0 ]]; then skipped=$((skipped + 1)); continue; fi
		fp=$(script_line_fp "$inp" "$cmd")
		key=scripts.$(printf '%s' "$cmd" | { shasum -a 256 2>/dev/null || sha256sum; } | cut -c1-16)
		if gate_reusable "$key" "$fp"; then
			echo "ok   ${cmd%% *} (reused)"
			reused=$((reused + 1))
			continue
		fi
		run+=("$cmd") run_fp+=("$fp") run_key+=("$key")
	done < <(paths_script_checks)
	if [[ ${#run[@]} -gt 0 ]]; then
		# shellcheck disable=SC2016 # the script is for the child shell
		for cmd in "${run[@]}"; do
			printf '%s\0%s\0' "$i" "$cmd"
			i=$((i + 1))
		done | xargs -0 -n2 -P "${KOINON_SCRIPTS_JOBS:-4}" bash -c 'scripts_one "$1" "$2"' _
		for ((i = 0; i < ${#run[@]}; i++)); do
			cmd=${run[$i]}
			name=${cmd%% *}
			if [[ $(cat "$SCRIPTS_RES/$i.rc" 2>/dev/null || echo 1) != 0 ]]; then
				echo "FAIL $cmd"
				tail -n 5 "$SCRIPTS_RES/$i.out" 2>/dev/null | sed 's/^/  | /'
				failed=1
			else
				echo "ok   $name"
				gate_record "${run_key[$i]}" "${run_fp[$i]}"
			fi
		done
	fi
	echo "scripts: ${#run[@]} run, $reused reused, $skipped of $n not selected (inputs unchanged)"
	rm -rf "$SCRIPTS_RES"
	return "$failed"
}
g_gitleaks() { gitleaks git --redact --no-banner --log-opts="$base..HEAD" .; }

mkdir -p "$logdir"
# One pid file per gate (#990): the gates this run will run, each claimed up front, so a parallel
# preflight of other gates (hack/lane-gates.sh runs one per gate) does not block it and --stop finds
# every run. A live holder of the same gate is the only conflict. The file is created with noclobber:
# of two runs that start at once, one wins.
plan=""
for g in $ALL; do
	[[ -n $(selected "$g") ]] && plan="$plan $g"
done
claimed=""
release_gates() {
	local g
	for g in $claimed; do
		[[ $(cat "$logdir/preflight.$g.pid" 2>/dev/null) == "$$" ]] && rm -f "$logdir/preflight.$g.pid"
	done
	return 0
}
trap release_gates EXIT
claim_gate() { # <gate>: success when this run now holds the gate's pid file; prints the holder's pid when it does not
	local pf=$logdir/preflight.$1.pid old
	( set -C; echo $$ >"$pf" ) 2>/dev/null && return 0
	old=$(cat "$pf" 2>/dev/null || true)
	if [[ -n $old && $old != "$$" ]] && kill -0 "$old" 2>/dev/null; then
		echo "$old"
		return 1
	fi
	rm -f "$pf" # a stale file of a run that died
	( set -C; echo $$ >"$pf" ) 2>/dev/null
}
for g in $plan; do
	if holder=$(claim_gate "$g"); then
		claimed="$claimed $g"
	else
		echo "preflight: the $g gate is already in progress for $root (run ${holder:-unknown}); stop it with hack/preflight.sh --stop (one pid file per gate: runs of other gates do not conflict)" >&2
		exit 2
	fi
done
failed="" incomplete="" ran=0
mb_all=$(git merge-base "$base" HEAD 2>/dev/null || echo "$base")
# Gates whose verdict depends on more than the files (time, the network, commit history) never reuse one.
reuse_fp() { # <gate>: prints the inputs fingerprint when the gate may reuse a verdict
	case $1 in e2e | gitleaks | vuln | deps | fuzz) return 0 ;; esac
	gate_reuse_enabled || return 0
	case $1 in
	generate | chart | web | tidy | scripts) gate_fingerprint job "$1" "$mb_all" ;;
	*) gate_fingerprint gate "$1" "$mb_all" ;;
	esac
}

# web_deps: web/node_modules, which `make generate` (buf runs web/node_modules/.bin/protoc-gen-es) and
# the web gate both need. A worktree made before hack/wt.sh installed them, or one whose install failed,
# gets one offline attempt from the pnpm store (no network). Still missing: the caller reports a skip,
# which lane-gates turns into notrun, never fail (#541).
web_deps() {
	[[ -e web/node_modules/.bin/protoc-gen-es ]] && return 0
	command -v pnpm >/dev/null 2>&1 || return 1
	[[ -f web/pnpm-lock.yaml ]] && pnpm --dir web install --offline --frozen-lockfile >/dev/null 2>&1
	[[ -e web/node_modules/.bin/protoc-gen-es ]]
}

run_gate() { # <gate>
	local g=$1 log=$logdir/$1.log start rc fp
	case $g in
	web) command -v pnpm >/dev/null || { echo "web skip (pnpm not installed)"; return; }
		web_deps || { echo "web skip (web dependencies missing; offline pnpm install failed)"; return; } ;;
	generate) web_deps || { echo "generate skip (web dependencies missing; offline pnpm install failed)"; return; } ;;
	proto) command -v buf >/dev/null || { echo "proto skip (buf not installed)"; return; } ;;
	shell) command -v shellcheck >/dev/null || { echo "shell skip (shellcheck not installed)"; return; } ;;
	actionlint) command -v actionlint >/dev/null || { echo "actionlint skip (actionlint not installed)"; return; } ;;
	fuzz) command -v go >/dev/null || { echo "fuzz skip (go not installed; CI runs hack/go-fuzz.sh)"; return; } ;;
	vuln) command -v go >/dev/null || { echo "vuln skip (go not installed; CI runs govulncheck $GOVULNCHECK_VERSION, hack/versions.env)"; return; } ;;
	zizmor) command -v zizmor >/dev/null || { echo "zizmor skip (zizmor not installed; CI pins $ZIZMOR_VERSION: brew install zizmor, or the release binary, see hack/versions.env)"; return; }
		have=$(zizmor --version 2>/dev/null | awk '{print $2}')
		[[ $have == "$ZIZMOR_VERSION" ]] || echo "zizmor warn: CI pins $ZIZMOR_VERSION (hack/versions.env), this is ${have:-unknown}; findings can differ" ;;
	esac
	ran=$((ran + 1))
	start=$SECONDS
	fp=$(reuse_fp "$g")
	if [[ -n $fp ]] && gate_reusable "$g" "$fp"; then
		echo "$g ok 0s (reused: inputs unchanged since the last green run; --no-reuse to run it)"
		return
	fi
	if [[ $g == e2e ]]; then
		hack/needs-e2e.sh "$base" >"$log" 2>&1
		rc=$?
		case $rc in
		0) echo "e2e info needed (hack/e2e-remote.sh --local on your Docker; or the e2e label)" ;;
		1) echo "e2e info not needed" ;;
		*) echo "e2e info unknown, treat as needed (see $log)" ;;
		esac
		return
	fi
	( set -e; "g_$g" ) >"$log" 2>&1
	rc=$?
	if [[ $rc -eq 0 ]]; then
		echo "$g ok $((SECONDS - start))s"
		[[ -n $fp ]] && gate_record "$g" "$fp"
		[[ $verbose == 1 ]] && sed 's/^/  | /' "$log"
	elif [[ $rc -eq 3 ]]; then
		# Exit 3 is the missing-requirement code (go-test, chart-check when helm cannot run, …).
		local names
		names=$(sed -n 's/.*INCOMPLETE (missing \([^)]*\)).*/\1/p' "$log" | tail -1)
		if [[ -z $names ]] && grep -qiE 'cannot run .*helm|helm: .*not found|operation not permitted' "$log"; then
			names=helm
		fi
		echo "$g INCOMPLETE $((SECONDS - start))s (skipped: ${names:-see log}) log=$log"
		if [[ $verbose == 1 ]]; then sed 's/^/  | /' "$log"; else tail -n 5 "$log" | sed 's/^/  | /'; fi
		incomplete="$incomplete${incomplete:+,}$g"
	else
		echo "$g FAIL $((SECONDS - start))s log=$log"
		if [[ $verbose == 1 ]]; then sed 's/^/  | /' "$log"; else tail -n 5 "$log" | sed 's/^/  | /'; fi
		failed="$failed${failed:+,}$g"
	fi
}

for g in $plan; do
	run_gate "$g"
done

if [[ -n $failed ]]; then
	echo "preflight FAIL ($failed)"
	exit 1
fi
if [[ -n $incomplete ]]; then
	echo "preflight incomplete ($incomplete: requirements missing, see above)"
	exit 3
fi
echo "preflight ok ($ran gates)"
