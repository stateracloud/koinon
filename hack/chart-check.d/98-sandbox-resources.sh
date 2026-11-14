# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: sandbox resources (ADR 0083).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# The reconciler reads the sandbox sizing from config.yaml's sandboxResources: the default, the
# bounds and the Kata memory rule. The defaults are the numbers of internal/sandboxres.DefaultPolicy.
cfg=templates/config.yaml
has "the default CPU is 250m request, 2 limit" 'cpu: \{request: "250m", limit: "2"\}' --show-only $cfg
has "the default memory is 1Gi request, 2Gi limit" 'memory: \{request: "1Gi", limit: "2Gi"\}' --show-only $cfg
has "the CPU request bound is 100m to 2" 'request: \{min: "100m", max: "2"\}' --show-only $cfg
has "the CPU limit bound ends at 4" 'limit: \{min: "100m", max: "4"\}' --show-only $cfg
has "the memory bounds are 512Mi to 8Gi" 'request: \{min: "512Mi", max: "8Gi"\}' --show-only $cfg
has "the memory limit bound is 512Mi to 8Gi" 'limit: \{min: "512Mi", max: "8Gi"\}' --show-only $cfg
has "the memory request equals the limit by default" 'memoryRequestEqualsLimit: true' --show-only $cfg

# An override reaches the reconciler.
sbx=(
  --set sandbox.resources.default.cpu.request=500m
  --set sandbox.resources.default.cpu.limit=3
  --set sandbox.resources.default.memory.request=2Gi
  --set sandbox.resources.default.memory.limit=3Gi
  --set sandbox.resources.bounds.cpu.limit.max=6
  --set sandbox.resources.bounds.memory.request.min=256Mi
  --set sandbox.resources.bounds.memory.limit.max=16Gi
  --set sandbox.resources.memoryRequestEqualsLimit=false
)
has "an override of the default CPU renders" 'cpu: \{request: "500m", limit: "3"\}' "${sbx[@]}" --show-only $cfg
has "an override of the default memory renders" 'memory: \{request: "2Gi", limit: "3Gi"\}' "${sbx[@]}" --show-only $cfg
has "an override of the CPU limit bound renders" 'limit: \{min: "100m", max: "6"\}' "${sbx[@]}" --show-only $cfg
has "an override of the memory request minimum renders" 'request: \{min: "256Mi", max: "8Gi"\}' "${sbx[@]}" --show-only $cfg
has "an override of the memory limit maximum renders" 'limit: \{min: "512Mi", max: "16Gi"\}' "${sbx[@]}" --show-only $cfg
has "memoryRequestEqualsLimit can be turned off" 'memoryRequestEqualsLimit: false' "${sbx[@]}" --show-only $cfg
lacks "the override replaces the default CPU" 'cpu: \{request: "250m"' "${sbx[@]}" --show-only $cfg

# A plain number is a quantity too, and an unknown key is refused by the values schema.
has "a numeric CPU renders as a string" 'limit: "8"' --set sandbox.resources.default.cpu.limit=8 --show-only $cfg
if render --set sandbox.resources.memoryRequestEqualsLimit=maybe >/dev/null 2>&1; then fail "a non-boolean memoryRequestEqualsLimit must not render"; fi
if render --set sandbox.resources.default.gpu.limit=1 >/dev/null 2>&1; then fail "an unknown sandbox.resources.default key must not render"; fi

# koinon.validate: the two cross-field rules of sandboxres.Policy.Validate (#664).
# The defaults pass.
render >/dev/null 2>&1 || fail "the default sandbox.resources values must render"

# Memory limit maximum above the request maximum: refused with the rule on, accepted with it off.
if sres_msg=$(render --set sandbox.resources.bounds.memory.limit.max=16Gi --set sandbox.resources.memoryRequestEqualsLimit=true 2>&1); then
	fail "memory.limit.max=16Gi with memoryRequestEqualsLimit=true must fail the render"
fi
grep -Fq 'memoryRequestEqualsLimit=true limit.max "16Gi" is above request.max "8Gi"' <<<"$sres_msg" ||
	fail "the memoryRequestEqualsLimit refusal does not name the values: $(head -c 300 <<<"$sres_msg")"
render --set sandbox.resources.bounds.memory.limit.max=16Gi --set sandbox.resources.memoryRequestEqualsLimit=false >/dev/null 2>&1 ||
	fail "memory.limit.max=16Gi with memoryRequestEqualsLimit=false must render"

# A limit range that starts below the request minimum: refused for memory and for cpu.
if sres_msg=$(render --set sandbox.resources.bounds.memory.limit.min=256Mi 2>&1); then
	fail "memory.limit.min below memory.request.min must fail the render"
fi
grep -Fq 'limit.min "256Mi" is below request.min "512Mi"' <<<"$sres_msg" || fail "the memory limit.min refusal does not name the values"
if sres_msg=$(render --set sandbox.resources.bounds.cpu.limit.min=50m 2>&1); then
	fail "cpu.limit.min below cpu.request.min must fail the render"
fi
grep -Fq 'limit.min "50m" is below request.min "100m"' <<<"$sres_msg" || fail "the cpu limit.min refusal does not name the values"

# Units compare as quantities, not as text: 8192Mi equals 8Gi, 1000m equals 1.
render --set sandbox.resources.bounds.memory.limit.max=8192Mi >/dev/null 2>&1 || fail "memory.limit.max=8192Mi equals the 8Gi request maximum and must render"
render --set sandbox.resources.bounds.cpu.limit.min=0.1 >/dev/null 2>&1 || fail "cpu.limit.min=0.1 equals 100m and must render"

# A value that is no quantity fails the render too.
if render --set sandbox.resources.bounds.memory.limit.max=lots >/dev/null 2>&1; then fail "a bound that is not a quantity must fail the render"; fi
