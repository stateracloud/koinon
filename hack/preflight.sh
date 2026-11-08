#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs the repo gates that your change can affect, before you push. The gates are the ones CI and
# AGENTS.md name; this picks them from the changed files so nobody runs them by hand one by one.
#
#   hack/preflight.sh [--base origin/main] [--only g1,g2] [--list] [-v]
#   hack/preflight.sh --stop
#
#   --base REF    compare against REF (default origin/main); the changed files are
#                 `git diff --name-only REF...HEAD` plus uncommitted and untracked files
#   --only LIST   run exactly these gates (comma list), whether or not the change triggers them
#   --list        print which gates would run and why, then exit
#   -v            print each gate's full output
#   --stop        stop this worktree's running preflight (pid file in its log directory) and exit;
#                 never touches another worktree's run. Do not pkill/pgrep by script name instead.
#
# Gates: go tidy deps generate proto migrate adr docs chart web shell actionlint mktemp bash5 audit versions e2e gitleaks.
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

ALL="go tidy deps generate proto migrate adr docs chart web shell actionlint mktemp bash5 audit versions e2e gitleaks"
# shellcheck source=/dev/null
. "$(dirname "$0")/versions.env" # GOFUMPT_VERSION, dated by hack/dep-age-check.sh (ADR 0079)
base=origin/main only="" list=0 verbose=0 stop=0
while [[ $# -gt 0 ]]; do
	case $1 in
	--base) [[ $# -ge 2 ]] || { echo "preflight: --base needs a ref" >&2; exit 2; }; base=$2; shift 2 ;;
	--only) [[ $# -ge 2 ]] || { echo "preflight: --only needs a list" >&2; exit 2; }; only=$2; shift 2 ;;
	--list) list=1; shift ;;
	-v) verbose=1; shift ;;
	--stop) stop=1; shift ;;
	-h | --help) sed -n 4,26p "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) echo "preflight: unknown argument $1 (see --help)" >&2; exit 2 ;;
	esac
done

cd "$(git rev-parse --show-toplevel)" || exit 2
# One log directory and pid file per worktree: a hash of the repo root keys it.
root=$(pwd -P)
rootkey=$(printf '%s' "$root" | { shasum -a 256 2>/dev/null || sha256sum; } | cut -c1-12)
logdir=${TMPDIR:-/tmp}/koinon-preflight/$rootkey
pidfile=$logdir/preflight.pid

kill_tree() { # <pid>: SIGTERM the process and its descendants (found by parent pid, never by name)
	local c
	for c in $(pgrep -P "$1" 2>/dev/null); do kill_tree "$c"; done
	kill "$1" 2>/dev/null || true
}

if [[ $stop == 1 ]]; then
	pid=$(cat "$pidfile" 2>/dev/null || true)
	if [[ -n $pid ]] && kill -0 "$pid" 2>/dev/null; then
		kill_tree "$pid"
		rm -f "$pidfile"
		echo "preflight: stopped run $pid of $root"
	else
		rm -f "$pidfile"
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

# reason <gate>: why the change triggers the gate; empty when it does not.
reason() {
	local n
	case $1 in
	go) n=$(count '\.go$'); [[ $n -gt 0 ]] && echo "$n Go files changed" ;;
	tidy) match '\.go$|^go\.(mod|sum)$' && echo "Go files or go.mod/go.sum changed" ;;
	deps) match '(^|/)(go\.mod|go\.sum|package\.json|pnpm-lock\.yaml|package-lock\.json|yarn\.lock|Dockerfile[^/]*)$' && echo "dependency manifests, lockfiles or Dockerfiles changed" ;;
	proto) match '\.proto$' && echo "proto files changed" ;;
	generate) match '\.proto$|^internal/db/queries/.*\.sql$|^sqlc\.yaml$' && echo "proto or sqlc queries changed" ;;
	migrate) match '^internal/db/migrations/' && echo "migrations changed" ;;
	adr) match '^(docs/adr/|hack/adr-)' && echo "docs/adr or the ADR scripts changed" ;;
	docs) match '^(docs/|test/e2e/|charts/|web/e2e-real/|hack/docs-|README\.md)' && echo "docs or what docs-check checks changed" ;;
	chart) match '^(charts/|hack/chart-check\.sh|hack/chart-check\.d/|hack/chartlint/)' && echo "charts changed" ;;
	web) match '^web/' && echo "web/ changed" ;;
	shell) n=$(count '\.sh$'); [[ $n -gt 0 ]] && echo "$n shell scripts changed" ;;
	actionlint) match '^\.github/(workflows/[^/]*\.ya?ml|actionlint\.ya?ml)$' && echo "workflows or the actionlint config changed" ;;
	mktemp) match '\.sh$|^Makefile$|^\.github/workflows/[^/]*\.yml$' && echo "shell scripts, the Makefile or workflows changed" ;;
	bash5) n=$(count '\.sh$'); [[ $n -gt 0 ]] && echo "$n shell scripts changed" ;;
	audit) match '\.go$|^internal/db/queries/.*\.sql$|^hack/audit-batch-lint' && echo "Go code, sqlc queries or the audit batch lint changed" ;;
	versions) match '^(hack/versions|hack/e2e-toolchain/|CONTRIBUTING\.md$|\.github/workflows/[^/]*\.yml$|hack/chart-check\.sh$|hack/[^/]*\.sh$|test/e2e/.*\.sh$)' && echo "the versions file or a copy of one changed" ;;
	e2e) echo "informational, always" ;;
	gitleaks) echo "always" ;;
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

go_pkgs() { # changed Go packages as ./dir args; every package (minus test/flavors) when no Go file changed;
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
		go list ./... | grep -v '/test/flavors$' | tr '\n' ' '
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
	buf breaking --against ".git#branch=${base#origin/}"
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
g_adr() { hack/adr-lint-test.sh >/dev/null && hack/adr-lint.sh; }
g_docs() { hack/docs-check.sh; }
g_chart() { hack/chart-check.sh; }
g_web() { (cd web && pnpm lint && pnpm typecheck && pnpm test); }
g_shell() {
	local files
	files=$(existing '\.sh$' | grep -v '\.golden\.sh$' || true) # the golden file is generated (hack/docs-check.sh)
	[[ -n $files ]] || { echo "no existing shell files"; return 0; }
	# shellcheck disable=SC2086
	shellcheck $files
}
g_actionlint() { actionlint; } # the whole tree, as CI does: a workflow's lint can depend on its siblings
g_mktemp() { # independent of shellcheck, so it runs where shellcheck is missing (old issue 535)
	hack/mktemp-lint-test.sh
	hack/mktemp-lint.sh
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
g_gitleaks() { gitleaks git --redact --no-banner --log-opts="$base..HEAD" .; }

mkdir -p "$logdir"
oldpid=$(cat "$pidfile" 2>/dev/null || true)
if [[ -n $oldpid ]] && kill -0 "$oldpid" 2>/dev/null; then
	echo "preflight: a run ($oldpid) is already in progress for $root (hack/preflight.sh --stop)" >&2
	exit 2
fi
echo $$ >"$pidfile"
trap 'rm -f "$pidfile"' EXIT
failed="" incomplete="" ran=0

run_gate() { # <gate>
	local g=$1 log=$logdir/$1.log start rc
	case $g in
	web) command -v pnpm >/dev/null || { echo "web skip (pnpm not installed)"; return; } ;;
	proto) command -v buf >/dev/null || { echo "proto skip (buf not installed)"; return; } ;;
	shell) command -v shellcheck >/dev/null || { echo "shell skip (shellcheck not installed)"; return; } ;;
	actionlint) command -v actionlint >/dev/null || { echo "actionlint skip (actionlint not installed)"; return; } ;;
	esac
	ran=$((ran + 1))
	start=$SECONDS
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
		[[ $verbose == 1 ]] && sed 's/^/  | /' "$log"
	elif [[ $g == go && $rc -eq 3 ]]; then
		local names
		names=$(sed -n 's/.*INCOMPLETE (missing \([^)]*\)).*/\1/p' "$log" | tail -1)
		echo "$g INCOMPLETE $((SECONDS - start))s (skipped: ${names:-see log}) log=$log"
		if [[ $verbose == 1 ]]; then sed 's/^/  | /' "$log"; else tail -n 5 "$log" | sed 's/^/  | /'; fi
		incomplete="$incomplete${incomplete:+,}$g"
	else
		echo "$g FAIL $((SECONDS - start))s log=$log"
		if [[ $verbose == 1 ]]; then sed 's/^/  | /' "$log"; else tail -n 5 "$log" | sed 's/^/  | /'; fi
		failed="$failed${failed:+,}$g"
	fi
}

for g in $ALL; do
	[[ -n $(selected "$g") ]] && run_gate "$g"
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
