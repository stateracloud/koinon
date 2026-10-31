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
