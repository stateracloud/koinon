#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The gateway load suite (issue 280): drives the fake MCP server directly, through agentgateway and
# through koinon-gateway with the same calls, and writes the numbers to a directory.
#
# It runs in the cloud only (ADR 0097 step 2): never as a required CI job, never from `make test`,
# never on a developer Mac. The Go files of the suite have the build tag "load" and the Ginkgo label
# "load", so no other test command compiles or runs them. Needs Docker (agentgateway runs in a
# container; its image digest is the chart's) and a Linux machine.
#
# Usage: hack/load-gateway.sh [--quick] [--out DIR]
#   --quick    3 seconds per level and levels 1 and 16, to check that the suite runs (not a baseline)
#   --out DIR  the output directory (default load-results/<UTC time>)
# Settings (environment): KOINON_LOAD_DURATION (default 15s), KOINON_LOAD_CONCURRENCY (default
# 1,16,64,256). Output: environment.txt (the machine), results.jsonl and report.md.
# No cluster: every command here runs with KUBECONFIG=/dev/null.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=
while [[ $# -gt 0 ]]; do
	case $1 in
	--quick)
		export KOINON_LOAD_DURATION=3s KOINON_LOAD_CONCURRENCY=1,16
		shift
		;;
	--out)
		out=${2:?--out needs a directory}
		shift 2
		;;
	-h | --help)
		sed -n '2,/^set -e/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "load-gateway: unknown argument $1" >&2
		exit 2
		;;
	esac
done

if [[ $(uname -s) != Linux ]]; then
	echo "load-gateway: the load suite runs in the cloud on Linux, never on a developer machine ($(uname -s))" >&2
	exit 2
fi
if ! command -v docker >/dev/null || ! docker info >/dev/null 2>&1; then
	echo "load-gateway: Docker is needed (agentgateway runs in a container)" >&2
	exit 2
fi

out=${out:-$repo/load-results/$(date -u +%Y%m%dT%H%M%SZ)}
mkdir -p "$out"
out=$(cd "$out" && pwd)
export KOINON_LOAD_OUT=$out KUBECONFIG=/dev/null
: "${KOINON_LOAD_DURATION:=15s}" "${KOINON_LOAD_CONCURRENCY:=1,16,64,256}"
export KOINON_LOAD_DURATION KOINON_LOAD_CONCURRENCY

{
	echo "date:        $(date -u +%FT%TZ)"
	echo "commit:      $(git -C "$repo" rev-parse HEAD)"
	echo "kernel:      $(uname -sr)"
	echo "cpu:         $(grep -m1 'model name' /proc/cpuinfo | sed 's/.*: //') x $(nproc)"
	echo "memory:      $(awk '/MemTotal/ {printf "%.1f GiB", $2/1048576}' /proc/meminfo)"
	echo "go:          $(go version)"
	echo "docker:      $(docker version --format '{{.Server.Version}}')"
	echo "agentgateway: $(grep -o 'sha256:[0-9a-f]*' "$repo/internal/testing/agentgatewayimage/image.go" | head -1)"
	echo "duration:    $KOINON_LOAD_DURATION per level"
	echo "levels:      $KOINON_LOAD_CONCURRENCY"
} >"$out/environment.txt"

# One package per driver; the same fake MCP server (cmd/koinon-fakemcp) behind each. -p 1: one run
# at a time, so the load of one driver never competes with another's.
cd "$repo"
{
	for pkg in ./internal/gateway/loadtest ./internal/gateway/standalone ./internal/cmd/koinongateway; do
		echo "## $pkg"
		go test -v -p 1 -count=1 -timeout 60m -tags load "$pkg" -ginkgo.label-filter=load -ginkgo.no-color 2>&1 |
			grep -v ' INFO ' || true
		echo
	done
} | tee "$out/run.log" | sed -n '/^| driver/,/^[^|]/{/^|/p};/^## /p;/^--- FAIL/p;/^FAIL/p' | tee "$out/report.md"
if grep -q '^FAIL\|^--- FAIL' "$out/run.log"; then
	echo "load-gateway: a suite failed; see $out/run.log" >&2
	exit 1
fi
echo "load-gateway: results in $out"
