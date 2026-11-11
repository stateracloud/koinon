#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints the directories (./relative, one per line) of the given packages (default ./...) whose test
# results may be reused from Go's test cache (issue 43, docs/dev/ci.md#go-test-suites).
#
# Go's test cache keys a result on the test binary, the command line, the environment variables the
# test read and the files it opened. It does not see Docker or testcontainers state, a database
# server, the network, a cluster or an external binary a test starts. A package can cache only when
# nothing it can reach has such an input:
#   - its test binary does not link internal/testing/* (pgtest, testenv, containerhost, ...) except
#     fakemodel, an in-process fake;
#   - it does not link testcontainers-go or controller-runtime's envtest;
#   - the package or its tests do not import os/exec (git, helm, kubectl, ... are not tracked). This checks
#     only the package's own imports and its in-module imports, not third-party dependencies that can exec.
# The rule over-approximates on purpose: a package that is wrongly kept out only loses a cache hit,
# one that is wrongly let in could report a stale pass. hack/go-test-test.sh keeps the rule honest.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -euo pipefail
cd "$(dirname "$0")/.."

pkgs=("$@")
((${#pkgs[@]})) || pkgs=(./...)
# Listed first: a failing `go list` must fail the script, not print an empty (all impure) list.
listing=$(go list -test -f '{{.Dir}}|{{.ForTest}}|{{.ImportPath}}|{{join .Imports " "}}|{{join .Deps " "}}' "${pkgs[@]}") || { echo "go-test-cacheable.sh: go list ${pkgs[*]} failed" >&2; exit 1; }

awk -F'|' -v pwd="$PWD" '
	function rel(d) { return index(d, pwd) == 1 ? "." substr(d, length(pwd) + 1) : d }
	{
		dir = rel($1)
		if ($2 != "") {
			# the package or its external test package, built for the test
			if (" " $4 " " ~ / os\/exec /) impure[dir] = 1
		} else if ($3 ~ /\.test$/) {
			hastest[dir] = 1
			deps = " " $5 " "
			gsub(/\/internal\/testing\/fakemodel /, " ", deps)
			if (deps ~ /\/internal\/testing\// || dir ~ /^\.\/internal\/testing\/(pgtest|testenv|containerhost|agentgatewayimage|cfgdeliver|fakegitea|policycheck|racescale)/) impure[dir] = 1
			if (deps ~ /github.com\/testcontainers\// || deps ~ /controller-runtime\/pkg\/envtest /) impure[dir] = 1
		}
	}
	END { for (d in hastest) if (!(d in impure)) print d }
' <<<"$listing" | sort
