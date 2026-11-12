#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests the one source of the path rules (hack/paths.rules, read through hack/paths-lib.sh) and its
# consumers on example change sets, so that the scripts cannot drift apart:
#   1. the reader rejects a bad rules file (unknown kind, bad regular expression, missing field);
#   2. the jobs of the rules are the path outputs of the `changes` job in ci.yml, and each has a lane;
#   3. every `run: hack/...` step of the quick checks and scripts jobs of ci.yml is a script-check
#      (run by the preflight gate `scripts`) or a script-skip (with a reason in the rules file);
#   4. example change sets give the same answer in hack/ci-changes.sh and hack/needs-e2e.sh as the
#      rules say (needs the Go toolchain for needs-e2e.sh's `go list`).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."

# First, and on its own line so set -e stops the script when mktemp fails (old issue 476).
tmp=$(mktemp -d "${TMPDIR:-/tmp}/paths-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fails=0 total=0
ok() { total=$((total + 1)); }
bad() { total=$((total + 1)); fails=$((fails + 1)); echo "FAIL: $*"; }

# shellcheck source=/dev/null
. hack/paths-lib.sh
paths_load || { echo "FAIL: hack/paths.rules does not load"; exit 1; }

# --- 1. A bad rules file is an error, never "no rule". -----------------------------------------------
loads() { # <name> <want 0|2> <rules file content>
	local f=$tmp/$1.rules rc=0
	printf '%s\n' "$3" >"$f"
	(HACK_PATHS_RULES=$f paths_load) 2>/dev/null || rc=$?
	if [[ $rc -eq $2 ]]; then ok; else bad "$1: paths_load exit $rc, want $2"; fi
}
good=$'everything - ^Makefile$\njob go .'
loads good 0 "$good"
loads unknown-kind 2 "$good"$'\nfrobnicate x y'
loads bad-regex 2 "$good"$'\njob web ^(web/'
loads no-pattern 2 "$good"$'\njob web'
loads no-everything 2 'job go .'
loads no-job 2 'everything - ^Makefile$'
if (HACK_PATHS_RULES=$tmp/missing.rules paths_load) 2>/dev/null; then bad "a missing rules file loads"; else ok; fi

# --- 2. The rules and ci.yml name the same jobs. -------------------------------------------------------
want=$(paths_jobs | sort | tr '\n' ' ')
have=$(sed -n '/^  changes:/,/^    steps:/p' .github/workflows/ci.yml | sed -n 's/^      \([a-z]*\): \${{ steps.paths.outputs.\1 }}$/\1/p' | grep -vx flavors_publish | sort | tr '\n' ' ')
if [[ $want == "$have" ]]; then ok; else bad "jobs in hack/paths.rules [${want% }] differ from the paths outputs of ci.yml [${have% }]"; fi
for j in $(paths_jobs); do
	if [[ -n $(paths_raw lane "$j") ]]; then ok; else bad "job $j has no lane line (lane-gates would report it as not run)"; fi
done

# --- 3. preflight's `scripts` gate covers the script steps of ci.yml. ---------------------------------
listed=$( { paths_raw script-check -; paths_raw script-skip -; } | awk '{print $1}' | sort -u)
steps=$( { sed -n '/^  quick-checks:/,/^  web-lint:/p' .github/workflows/ci.yml; sed -n '/^  scripts:/,/^  warm-caches:/p' .github/workflows/ci.yml; } |
	sed -n -E -e 's/^ +(- )?run: (hack\/[A-Za-z0-9_.\/-]+)( [^$"]*)?$/\2/p' -e 's/^ {10,}(hack\/[A-Za-z0-9_.\/-]+\.sh)( .*)?$/\1/p' | sort -u) # also the lines of a `run: |` block
[[ $(grep -c . <<<"$steps") -ge 30 ]] || bad "parsed only $(grep -c . <<<"$steps") script steps from ci.yml (the parser is stale)"
while IFS= read -r st; do
	[[ -n $st ]] || continue
	if grep -qxF -- "$st" <<<"$listed"; then ok; else bad "ci.yml runs $st; add it to hack/paths.rules as script-check or script-skip"; fi
done <<<"$steps"
while IFS= read -r c; do
	[[ -n $c ]] || continue
	if [[ -x ${c%% *} ]]; then ok; else bad "script-check/script-skip names ${c%% *}, which is not an executable file"; fi
done < <( { paths_raw script-check -; paths_raw script-skip -; } | awk '{print $1}' | sort -u)
dups=$( { paths_raw script-check -; paths_raw script-skip -; } | awk '{print $1}' | sort | uniq -d | while read -r d; do
	# the same script with different arguments is fine in script-check; in both lists it is not
	if paths_raw script-check - | awk '{print $1}' | grep -qxF "$d" && paths_raw script-skip - | awk '{print $1}' | grep -qxF "$d"; then echo "$d"; fi
done)
if [[ -z $dups ]]; then ok; else bad "in both script-check and script-skip: $dups"; fi

# --- 4. Example change sets through the consumers. ---------------------------------------------------------
dirs=$(bash hack/ci-changes.sh --flavors-dirs)
jobs_for() { printf '%s\n' "$@" | CI_CHANGES_FLAVORS_DIRS=$dirs bash hack/ci-changes.sh --stdin | sed -n 's/^\(.*\)=true$/\1/p' | sort | tr '\n' ' '; }
e2e_for() { local rc=0; printf '%s\n' "$@" | bash hack/needs-e2e.sh --stdin >/dev/null || rc=$?; echo "$rc"; }
example() { # <name> <want jobs> <want e2e exit> <paths...>
	local name=$1 wj got ge
	wj=$(tr ' ' '\n' <<<"$2" | sed '/^$/d' | sort | tr '\n' ' ')
	local we=$3
	shift 3
	got=$(jobs_for "$@")
	ge=$(e2e_for "$@")
	if [[ $got == "$wj" && $ge == "$we" ]]; then ok; else bad "$name: jobs [${got% }] want [${wj% }]; e2e exit $ge want $we"; fi
}
example "one hack script" "scripts shell" 1 hack/ship.sh
example "a workflow other than ci.yml" "scripts shell" 1 .github/workflows/dco.yml
example "the Go test wrapper" "go scripts shell" 1 hack/go-test.sh
example "e2e driver script" "go scripts shell" 0 test/e2e/run.sh
example "a chart template" "chart chartsmoke escape go scripts" 0 charts/koinon/templates/api.yaml
example "a web source file" "web" 1 web/src/app.ts
example "a Go test file of the reconciler" "go scripts tidy" 1 internal/reconciler/reconcile_test.go
example "the rules themselves" "chart chartsmoke escape flavors fuzz generate go opencodeplugin scripts shell tidy vuln web" 1 hack/paths.rules
example "go.mod runs govulncheck" "chart chartsmoke escape flavors fuzz generate go opencodeplugin scripts shell tidy vuln web" 0 go.mod
example "a Go file alone does not run govulncheck" "chartsmoke go scripts tidy" 0 internal/reconciler/reconcile.go
example "documentation" "" 1 docs/design.md

# The `vuln` rule itself (`everything` turns every job on for go.mod and go.sum, so the changes-level tests
# cannot see a narrowed rule): go.mod and go.sum each trigger it, a nested spike module does not.
for f in go.mod go.sum hack/govulncheck.sh .github/workflows/govulncheck.yml hack/versions.env; do
	if paths_job_triggers vuln "$f"; then ok; else bad "the vuln rule must match $f"; fi
done
for f in hack/spike/acpdrive/go.mod hack/spike/acpdrive/go.sum internal/foo/go.mod docs/go.sum internal/x.go hack/govulncheck-test.sh; do
	if paths_job_triggers vuln "$f"; then bad "the vuln rule must not match $f"; else ok; fi
done
# The `fuzz` rule (#684): the fuzzed packages and the script, not every Go file.
for f in internal/leakguard/taint.go internal/mcpsanitize/mcpsanitize_test.go internal/tracing/scrub/jsonscrub.go hack/go-fuzz.sh; do
	if paths_job_triggers fuzz "$f"; then ok; else bad "the fuzz rule must match $f"; fi
done
for f in internal/mcpguard/result.go internal/tracing/tracing.go internal/leakguardx/a.go hack/go-fuzz-test.sh; do
	if paths_job_triggers fuzz "$f"; then bad "the fuzz rule must not match $f"; else ok; fi
done
example "the CI documentation, read by ci-cache-test" "scripts" 1 docs/dev/ci.md

if [[ $fails -ne 0 ]]; then
	echo "paths-test: $fails of $total checks failed"
	exit 1
fi
echo "paths-test: ok ($total checks)"
