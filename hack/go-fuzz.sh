#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs every Go fuzz target of the packages whose input is untrusted text, each for a fixed time (#684):
# the secret scrubber and its structural search (internal/tracing/scrub), the leak guard's normaliser,
# decoders and taint check (internal/leakguard) and the MCP tool sanitiser (internal/mcpsanitize). The
# seed corpora of these targets already run as plain tests in go-test; this script adds the mutation.
# The one way to run it: the advisory `fuzz` job of ci.yml (path rule `fuzz`), the preflight gate
# `fuzz` and by hand.
#
#   hack/go-fuzz.sh [-list]          FUZZTIME (default 20s) per target
#
# The targets are found, not listed: every `func FuzzX(` in a _test.go file of the packages below. A
# target that fails leaves its input under <package>/testdata/fuzz/FuzzX/; the job uploads it. Add the
# file to the change that fixes the cause, so the input becomes a seed that runs in every go-test run.
#
# Exit codes: 0 every target ran its time without a failure, 1 a target failed, 2 a usage or setup error.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

cd "$(dirname "$0")/.."

# Keep in step with the `fuzz` rule of hack/paths.rules.
packages=(internal/leakguard internal/mcpsanitize internal/tracing/scrub)
fuzztime=${FUZZTIME:-20s}

list=false
case ${1:-} in
'') ;;
-list) list=true ;;
*) echo "usage: hack/go-fuzz.sh [-list]" >&2; exit 2 ;;
esac

targets=()
for p in "${packages[@]}"; do
	[[ -d $p ]] || { echo "go-fuzz: no package $p" >&2; exit 2; }
	while IFS= read -r name; do
		targets+=("$p $name")
	done < <(grep -ho '^func Fuzz[A-Za-z0-9_]*(' "$p"/*_test.go | sed 's/^func //; s/($//' | sort -u)
done
[[ ${#targets[@]} -gt 0 ]] || { echo "go-fuzz: no fuzz target found" >&2; exit 2; }

if $list; then
	printf '%s\n' "${targets[@]}"
	exit 0
fi

failed=()
for t in "${targets[@]}"; do
	read -r p name <<<"$t"
	echo "=== $p $name ($fuzztime)"
	# -run '^$': only the fuzz target runs (with its seeds first). The fuzzing workers are GOMAXPROCS by
	# default, which is the runner's cores.
	if ! go test -run '^$' -fuzz "^${name}\$" -fuzztime "$fuzztime" "./$p"; then
		failed+=("$p $name")
		[[ -z ${GITHUB_ACTIONS:-} ]] || echo "::error title=go-fuzz::$name in $p failed; its input is in $p/testdata/fuzz/$name/"
	fi
done

if [[ ${#failed[@]} -gt 0 ]]; then
	echo "go-fuzz: FAILED: ${failed[*]}" >&2
	exit 1
fi
echo "go-fuzz: ${#targets[@]} targets, $fuzztime each, no failure"
