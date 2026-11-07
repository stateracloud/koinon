# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the RuntimeClasses the sandbox policy admits (ADR 0075, old issue 442).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

pol=templates/sandbox-policy.yaml
has "the default admits kata, kata-qemu and kata-qemu-runtime-rs" "object.spec.runtimeClassName in \[\"kata\",\"kata-qemu\",\"kata-qemu-runtime-rs\"\]" --show-only $pol
has "the denial message names the three classes" 'runtimeClassName kata or kata-qemu or kata-qemu-runtime-rs' --show-only $pol
has "an override of the list renders" 'object.spec.runtimeClassName in \["kata"\]' --set 'sandboxPolicy.runtimeClasses={kata}' --show-only $pol
lacks "runc is not admitted by default" '"runc"' --show-only $pol

# The policy, not the schema, refuses unknown classes: the default never lists runc or gvisor,
# and the list may name other classes (microvm, koinon-hardened) when an operator sets them.
lacks "gvisor is not admitted by default" '"gvisor"' --show-only $pol
if render --set 'sandboxPolicy.runtimeClasses={}' >/dev/null 2>&1; then fail "an empty RuntimeClass list must not render"; fi
render --set 'sandboxPolicy.runtimeClasses={microvm}' --set openshell.runtimeClassName=microvm >/dev/null 2>&1 || fail "a non-kata class (microvm) must render"

# The reconciler's class must be admitted by the policy: the chart refuses to render otherwise.
render --set openshell.runtimeClassName=kata-qemu-runtime-rs >/dev/null 2>&1 || fail "runtimeClassName kata-qemu-runtime-rs must render with the default list"
if gvisor_out=$(render --set openshell.runtimeClassName=gvisor 2>&1); then
	fail "runtimeClassName gvisor outside sandboxPolicy.runtimeClasses must not render"
else
	grep -Fq 'openshell.runtimeClassName "gvisor" is not in sandboxPolicy.runtimeClasses' <<<"$gvisor_out" || fail "the runtimeClassName guard message must name both values"
fi
