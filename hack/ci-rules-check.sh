#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Checks the CI rules that a text scan can decide (ADR 0111, docs/dev/ci-rules.md). One line per rule:
#
#   rule=<ID> verdict=pass|fail|report detail="..."
#
# Exit 1 when any rule fails, 0 otherwise. `report` never fails: it is a rule whose check is not yet
# blocking (the guide says which and when it turns on).
#
#   W1  every workflow has a top-level `permissions:` key.
#   W2  every `uses:` of a third-party action is pinned by a full 40-character commit SHA.
#   W5  no workflow reads a secret other than GITHUB_TOKEN (so no model key: T6, and none for fork code).
#   Q1  a workflow that defines a required check (names in hack/ci-required.txt) has no workflow-level
#       `paths:` or `paths-ignore:` filter under `on:`. The job-level `if:` form is fine.
#   Q6  a workflow that defines a required check never cancels runs of a queue or main build: a literal
#       `cancel-in-progress: true` fails; an expression must name `pull_request`.
#   T1  no automatic test retry: no ginkgo flake attempts, no retry action. Playwright `retries` above 0
#       is only reported until part 2 of #579 removes it.
#   T2  a flaky-test waiver `flaky-waiver: #<issue> until <YYYY-MM-DD>` is well formed and not past its date.
#   T7  every job of ci.yml sets `timeout-minutes`. Report mode while hack/runner-lint.sh has no timeout
#       rule; once it has (#572), that script is the check and this line says so.
#
# Not here, because another script already decides it: W3 (hack/runner-lint.sh), Q2 and Q3
# (hack/ci-required-test.sh, hack/ci-merge-group-test.sh), the 3-day age of dependencies and the
# action pin age (hack/dep-age-check.sh), migrations (hack/migrate-lint.sh).
#
# Prior art: zizmor (excessive-permissions, unpinned-uses audits) covers W1 and W2 more deeply; it
# arrives with part 2 of #579. This script keeps the Koinon-specific rules (Q1, Q6, T1, T2, T7) that no
# linter knows. actionlint does not check permissions or pins (github.com/rhysd/actionlint).
#
# Usage: hack/ci-rules-check.sh [root]    (default: the repository root)
# CI_RULES_TODAY=YYYY-MM-DD overrides today's date (the test uses it).

# shellcheck disable=SC2016 # '${{' is a literal GitHub expression opener, not a shell expansion

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

root=${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}
wfdir=$root/.github/workflows
list=$root/hack/ci-required.txt
today=${CI_RULES_TODAY:-$(date +%F)}
failed=0

# verdict ID pass|fail|report DETAIL
verdict() {
	printf 'rule=%s verdict=%s detail="%s"\n' "$1" "$2" "$3"
	[[ $2 == fail ]] && failed=1
	return 0
}

shopt -s nullglob
workflows=("$wfdir"/*.yml "$wfdir"/*.yaml)
shopt -u nullglob
if [[ ${#workflows[@]} -eq 0 ]]; then
	echo "ci-rules-check: no workflow under $wfdir" >&2
	exit 2
fi

# Drop comment-only lines so a comment cannot satisfy or break a rule.
code_lines() { grep -v -E '^[[:space:]]*#' "$1" || true; }

# --- W1 -------------------------------------------------------------------------------------------
bad=()
for f in "${workflows[@]}"; do
	code_lines "$f" | grep -q -E '^permissions:' || bad+=("$(basename "$f")")
done
if [[ ${#bad[@]} -eq 0 ]]; then
	verdict W1 pass "${#workflows[@]} workflows set top-level permissions"
else
	verdict W1 fail "no top-level permissions: ${bad[*]}"
fi

# --- W2 -------------------------------------------------------------------------------------------
bad=()
for f in "${workflows[@]}"; do
	while IFS= read -r line; do
		spec=${line#*uses:}
		spec=${spec#"${spec%%[![:space:]]*}"}
		spec=${spec%%[[:space:]#]*}
		spec=${spec//\"/}
		spec=${spec//\'/}
		[[ $spec == ./* || $spec == docker://* ]] && continue
		[[ $spec =~ @[0-9a-f]{40}$ ]] || bad+=("$(basename "$f"): $spec")
	done < <(code_lines "$f" | grep -E '^[[:space:]-]*uses:[[:space:]]')
done
if [[ ${#bad[@]} -eq 0 ]]; then
	verdict W2 pass "every action is pinned by a full commit SHA"
else
	verdict W2 fail "not pinned by SHA: ${bad[*]}"
fi

# --- W5 -------------------------------------------------------------------------------------------
bad=()
for f in "${workflows[@]}"; do
	while IFS= read -r name; do
		[[ $name == secrets.GITHUB_TOKEN ]] || bad+=("$(basename "$f"): $name")
	done < <(code_lines "$f" | grep -o -E 'secrets\.[A-Za-z0-9_]+|secrets:[[:space:]]*inherit' || true)
done
if [[ ${#bad[@]} -eq 0 ]]; then
	verdict W5 pass "no workflow reads a secret besides GITHUB_TOKEN"
else
	verdict W5 fail "${bad[*]}"
fi

# --- Q1 and Q6: workflows that define a required check ---------------------------------------------
# Required names: every non-comment line of the list except `advisory ...`; the first word is a kind
# (`aggregator`, `external`) only when it is one of those two.
required=()
if [[ -f $list ]]; then
	while IFS= read -r line; do
		[[ -z $line || $line == \#* || $line == advisory\ * ]] && continue
		line=${line#aggregator }
		line=${line#external }
		required+=("$line")
	done <"$list"
fi

# defines_required FILE: succeeds when a job of FILE has a `name:` (4 spaces) or an id (2 spaces) in the list.
defines_required() {
	local n
	for n in "${required[@]}"; do
		local l
		while IFS= read -r l; do
			l=${l%%[[:space:]]#*}
			l=${l%"${l##*[![:space:]]}"}
			[[ $l == "    name: $n" || $l == "    name: $n:" || $l == "  $n" || $l == "  $n:" ]] && return 0
		done < <(code_lines "$1")
	done
	return 1
}

producers=()
for f in "${workflows[@]}"; do
	defines_required "$f" && producers+=("$f")
done

if [[ ${#required[@]} -eq 0 ]]; then
	verdict Q1 fail "no required check names found in hack/ci-required.txt"
	verdict Q6 fail "no required check names found in hack/ci-required.txt"
else
	bad=()
	for f in "${producers[@]}"; do
		# Under the top-level `on:` block, a key at 4 spaces named paths or paths-ignore is workflow-level.
		if code_lines "$f" | awk '
			/^on:/ { inon = 1; next }
			/^[^ \t]/ { inon = 0 }
			inon && /^    paths(-ignore)?:/ { hit = 1 }
			END { exit !hit }'; then
			bad+=("$(basename "$f")")
		fi
	done
	if [[ ${#bad[@]} -eq 0 ]]; then
		verdict Q1 pass "${#producers[@]} workflows with a required check have no workflow-level paths filter"
	else
		verdict Q1 fail "workflow-level paths filter on a workflow with a required check: ${bad[*]}"
	fi

	bad=()
	for f in "${producers[@]}"; do
		while IFS= read -r line; do
			val=${line#*cancel-in-progress:}
			val=${val#"${val%%[![:space:]]*}"}
			if [[ $val == true* ]]; then
				bad+=("$(basename "$f"): cancel-in-progress: true")
			elif [[ $val == '${{'* && $val != *"github.event_name == 'pull_request'"* ]]; then
				bad+=("$(basename "$f"): cancel-in-progress expression without github.event_name == 'pull_request'")
			fi
		done < <(code_lines "$f" | grep -E '^[[:space:]]+cancel-in-progress:')
	done
	if [[ ${#bad[@]} -eq 0 ]]; then
		verdict Q6 pass "no workflow with a required check cancels a queue or main run"
	else
		verdict Q6 fail "${bad[*]}"
	fi
fi

# --- T1 -------------------------------------------------------------------------------------------
# Scan the files that run tests; docs are excluded (they explain the rule), and so are this script and its test.
scan() { # PATTERN [grep options...]: matching lines of the scanned files
	local pat=$1
	shift
	grep -r -I -n -E "$@" --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=docs --exclude-dir=testdata \
		--exclude=ci-rules-check.sh --exclude=ci-rules-check-test.sh "$pat" "$root" 2>/dev/null || true
}
hits=$(scan 'flake-attempts|[Ff]lakeAttempts' --include='*.go' --include='*.sh' --include='*.yml' --include='*.yaml' --include='Makefile' --include='*.mk')
hits+=$'\n'$(scan 'nick-fields/retry|nick-invision/retry|Wandalen/wretry|wretry\.action|retry-action' --include='*.yml' --include='*.yaml')
if [[ -n ${hits//[[:space:]]/} ]]; then
	verdict T1 fail "automatic test retry: $(tr '\n' ' ' <<<"$hits" | cut -c1-300)"
else
	pw=()
	for f in "$root"/web/playwright*.ts; do
		[[ -f $f ]] || continue
		while IFS= read -r line; do
			val=${line#*retries:}
			val=${val%%,*}
			val=${val#"${val%%[![:space:]]*}"}
			[[ $val == 0 ]] || pw+=("$(basename "$f") retries: $val")
		done < <(grep -E '^[[:space:]]*retries:' "$f" || true)
	done
	if [[ ${#pw[@]} -eq 0 ]]; then
		verdict T1 pass "no automatic test retry"
	else
		verdict T1 report "Playwright retry still on, removed by part 2 of #579: ${pw[*]}"
	fi
fi

# --- T2 -------------------------------------------------------------------------------------------
bad=()
n=0
while IFS= read -r line; do
	[[ -z $line ]] && continue
	n=$((n + 1))
	where=${line%%:*}
	where=${where#"$root"/}
	if [[ $line =~ flaky-waiver:[[:space:]]+#([0-9]+)[[:space:]]+until[[:space:]]+([0-9]{4}-[0-9]{2}-[0-9]{2}) ]]; then
		[[ ${BASH_REMATCH[2]} < $today ]] && bad+=("$where: waiver for #${BASH_REMATCH[1]} expired ${BASH_REMATCH[2]}")
	else
		bad+=("$where: malformed waiver (want: flaky-waiver: #<issue> until <YYYY-MM-DD>)")
	fi
done < <(scan 'flaky-waiver:' --include='*.go' --include='*.ts' --include='*.tsx' --include='*.sh' --include='*.yml' --include='*.yaml')
if [[ ${#bad[@]} -eq 0 ]]; then
	verdict T2 pass "$n flaky-test waivers, none past its date"
else
	verdict T2 fail "${bad[*]}"
fi

# --- T7 -------------------------------------------------------------------------------------------
ci=$wfdir/ci.yml
if [[ -f $root/hack/runner-lint.sh ]] && code_lines "$root/hack/runner-lint.sh" | grep -q 'timeout-minutes'; then
	verdict T7 pass "hack/runner-lint.sh checks timeout-minutes on every ci.yml job"
elif [[ -f $ci ]]; then
	counts=$(code_lines "$ci" | awk '
		/^jobs:/ { injobs = 1; next }
		injobs && /^[^ \t]/ { injobs = 0 }
		!injobs { next }
		/^  [A-Za-z0-9_-]+:[ \t]*$/ { jobs++; next }
		/^    timeout-minutes:/ { tm++ }
		END { printf "%d %d", jobs + 0, tm + 0 }')
	jobs=${counts% *}
	tm=${counts#* }
	if ((jobs == tm)); then
		verdict T7 pass "all $jobs ci.yml jobs set timeout-minutes; move the check into hack/runner-lint.sh"
	else
		verdict T7 report "$((jobs - tm)) of $jobs ci.yml jobs have no timeout-minutes (becomes failing at zero)"
	fi
else
	verdict T7 report "no ci.yml"
fi

exit "$failed"
