# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: koinon.validate refuses the two cross-field sandbox.resources rules that koinon-api
# and the reconciler refuse to start on (sandboxres.Policy.Validate, #664).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, fail, ...).

# The defaults pass.
render >/dev/null 2>&1 || fail "the default sandbox.resources values must render"

# Memory limit maximum above the request maximum: refused with the rule on, accepted with it off.
sres_msg=$(render --set sandbox.resources.bounds.memory.limit.max=16Gi --set sandbox.resources.memoryRequestEqualsLimit=true 2>&1) &&
	fail "memory.limit.max=16Gi with memoryRequestEqualsLimit=true must fail the render"
grep -Fq 'memoryRequestEqualsLimit=true limit.max "16Gi" is above request.max "8Gi"' <<<"$sres_msg" ||
	fail "the memoryRequestEqualsLimit refusal does not name the values: $(head -c 300 <<<"$sres_msg")"
render --set sandbox.resources.bounds.memory.limit.max=16Gi --set sandbox.resources.memoryRequestEqualsLimit=false >/dev/null 2>&1 ||
	fail "memory.limit.max=16Gi with memoryRequestEqualsLimit=false must render"

# A limit range that starts below the request minimum: refused for memory and for cpu.
sres_msg=$(render --set sandbox.resources.bounds.memory.limit.min=256Mi 2>&1) &&
	fail "memory.limit.min below memory.request.min must fail the render"
grep -Fq 'limit.min "256Mi" is below request.min "512Mi"' <<<"$sres_msg" || fail "the memory limit.min refusal does not name the values"
sres_msg=$(render --set sandbox.resources.bounds.cpu.limit.min=50m 2>&1) &&
	fail "cpu.limit.min below cpu.request.min must fail the render"
grep -Fq 'limit.min "50m" is below request.min "100m"' <<<"$sres_msg" || fail "the cpu limit.min refusal does not name the values"

# Units compare as quantities, not as text: 8192Mi equals 8Gi, 1000m equals 1.
render --set sandbox.resources.bounds.memory.limit.max=8192Mi >/dev/null 2>&1 || fail "memory.limit.max=8192Mi equals the 8Gi request maximum and must render"
render --set sandbox.resources.bounds.cpu.limit.min=0.1 >/dev/null 2>&1 || fail "cpu.limit.min=0.1 equals 100m and must render"

# A value that is no quantity fails the render too.
render --set sandbox.resources.bounds.memory.limit.max=lots >/dev/null 2>&1 && fail "a bound that is not a quantity must fail the render"
true
