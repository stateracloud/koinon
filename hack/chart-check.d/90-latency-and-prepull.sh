# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: latency defaults, ask_user, harness pre-pull.
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# Latency defaults (plan item LT1): the reconciler's warm window and job bound reach its config.
has "reconciler config carries the park warm window" 'parkWarm: 15m' --show-only templates/config.yaml
# ask_user (plan item TK3): the reconciler offers the koinon target, served on the tools listener.
has "reconciler config names the ask_user tool server" 'koinonTools:' --show-only templates/config.yaml
has "the ask_user tool server is on the tools Service" 'svc:[0-9]+/mcp-koinon$' --show-only templates/config.yaml
has "reconciler config carries the park warm window override" 'parkWarm: 30m' --set reconciler.parkWarm=30m --show-only templates/config.yaml
has "reconciler config carries the archive window (AR1)" 'archiveAfter: 168h' --show-only templates/config.yaml
has "reconciler config carries the archive window override" 'archiveAfter: 24h' --set reconciler.archiveAfter=24h --show-only templates/config.yaml
has "reconciler config can turn the archive tier off" 'archiveAfter: -1h' --set reconciler.archiveAfter=-1h --show-only templates/config.yaml
has "reconciler config carries the sandbox job bound" 'sandboxJobs: 8' --show-only templates/config.yaml
has "reconciler config carries the keep-warm cap per org" 'keepWarmPerOrg: 2' --show-only templates/config.yaml
# Harness pre-pull (practice B6): off by default; needs a node selector; one init container per
# harness image through koinon.imageRef and the pull secrets; never in a sandbox namespace.
prepull=(--set harnessPrepull.enabled=true --set-string 'harnessPrepull.nodeSelector.katacontainers\.io/kata-runtime=true')
lacks "no harness pre-pull by default" 'kind: DaemonSet'
has "harness pre-pull renders a DaemonSet" '^kind: DaemonSet$' "${prepull[@]}" --show-only templates/harness-prepull.yaml
has "harness pre-pull selects the Kata nodes" 'katacontainers.io/kata-runtime: "true"' "${prepull[@]}" --show-only templates/harness-prepull.yaml
has "harness pre-pull pulls the harness image" 'image: "ghcr.io/stateracloud/koinon/flavors/opencode:' "${prepull[@]}" --show-only templates/harness-prepull.yaml
has "harness pre-pull image follows the registry mirror" 'image: "mirror.example.com/stateracloud/koinon/flavors/opencode:' "${prepull[@]}" \
	--set global.imageRegistry=mirror.example.com --show-only templates/harness-prepull.yaml
has "harness pre-pull pause image follows the registry mirror" 'image: "mirror.example.com/pause:3.10"' "${prepull[@]}" \
	--set global.imageRegistry=mirror.example.com --show-only templates/harness-prepull.yaml
has "harness pre-pull uses the image pull secrets" '- name: regcred' "${prepull[@]}" --set 'global.imagePullSecrets[0].name=regcred' \
	--show-only templates/harness-prepull.yaml
lacks "harness pre-pull sets no runtime class (it is not a sandbox)" 'runtimeClassName' "${prepull[@]}" --show-only templates/harness-prepull.yaml
lacks "harness pre-pull is not privileged" 'privileged: true' "${prepull[@]}" --show-only templates/harness-prepull.yaml
has "harness pre-pull drops all capabilities" 'drop: \[ALL\]' "${prepull[@]}" --show-only templates/harness-prepull.yaml
if render --set harnessPrepull.enabled=true >/dev/null 2>&1; then fail "harnessPrepull without a nodeSelector must not render"; fi

