#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# hack/go-test.sh runs its PARALLEL_SUITES with Ginkgo -p (#222). Fails when a listed suite is not
# a Ginkgo suite, when one starts Postgres outside a BeforeSuite (the container would then be shared
# or missing in the other processes), or when one binds a fixed local port.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

suites=$(hack/go-test.sh --list)
[[ -n $suites ]] || { echo "FAIL: hack/go-test.sh lists no parallel suites"; exit 1; }
for s in $suites; do
	[[ -d $s ]] || { echo "FAIL: $s is not a directory"; exit 1; }
	grep -lq 'RunSpecs(' "$s"/*_suite_test.go || { echo "FAIL: $s has no Ginkgo suite"; exit 1; }
	# A fixed port makes two processes of one suite collide.
	if grep -nE 'net\.Listen\("tcp", *"[^"]*:[1-9][0-9]*"|ListenAndServe\("[^"]*:[1-9][0-9]*"' "$s"/*_test.go; then
		echo "FAIL: $s binds a fixed port; use :0 so parallel processes do not collide"; exit 1
	fi
	# Package-level Postgres start (outside any node) would run once per process at init, before Ginkgo.
	if grep -nE '^(var|func init).*pgtest\.' "$s"/*_test.go; then
		echo "FAIL: $s starts Postgres at package level; do it in BeforeSuite"; exit 1
	fi
done
echo "ok: $(echo "$suites" | tr '\n' ' ')"
