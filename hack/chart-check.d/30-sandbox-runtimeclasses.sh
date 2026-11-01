# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the RuntimeClasses the sandbox policy admits (ADR 0075, #442).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

pol=templates/sandbox-policy.yaml
has "the default admits kata, kata-qemu and kata-qemu-runtime-rs" "object.spec.runtimeClassName in \[\"kata\",\"kata-qemu\",\"kata-qemu-runtime-rs\"\]" --show-only $pol
has "the denial message names the three classes" 'runtimeClassName kata or kata-qemu or kata-qemu-runtime-rs' --show-only $pol
has "an override of the list renders" 'object.spec.runtimeClassName in \["kata"\]' --set 'sandboxPolicy.runtimeClasses={kata}' --show-only $pol
lacks "runc is not admitted by default" '"runc"' --show-only $pol

# Only Kata classes (and runc for the dev tier) pass the schema; gvisor and an empty list do not.
if render --set 'sandboxPolicy.runtimeClasses={kata,gvisor}' >/dev/null 2>&1; then fail "a non-Kata RuntimeClass must not render"; fi
if render --set 'sandboxPolicy.runtimeClasses={katafake}' >/dev/null 2>&1; then fail "a name that only starts with kata must not render"; fi
if render --set 'sandboxPolicy.runtimeClasses={}' >/dev/null 2>&1; then fail "an empty RuntimeClass list must not render"; fi
render --set 'sandboxPolicy.runtimeClasses={kata-clh}' >/dev/null 2>&1 || fail "another Kata class (kata-clh) must render"
