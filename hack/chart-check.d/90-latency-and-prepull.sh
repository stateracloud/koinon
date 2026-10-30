# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: latency defaults, ask_user, flavor pre-pull.
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
# Flavor pre-pull (practice B6): off by default; needs a node selector; one init container per
# flavor image through koinon.imageRef and the pull secrets; never in a sandbox namespace.
prepull=(--set flavorPrepull.enabled=true --set-string 'flavorPrepull.nodeSelector.katacontainers\.io/kata-runtime=true')
lacks "no flavor pre-pull by default" 'kind: DaemonSet'
has "flavor pre-pull renders a DaemonSet" '^kind: DaemonSet$' "${prepull[@]}" --show-only templates/flavor-prepull.yaml
has "flavor pre-pull selects the Kata nodes" 'katacontainers.io/kata-runtime: "true"' "${prepull[@]}" --show-only templates/flavor-prepull.yaml
has "flavor pre-pull pulls the flavor image" 'image: "ghcr.io/stateracloud/flavor-opencode:' "${prepull[@]}" --show-only templates/flavor-prepull.yaml
has "flavor pre-pull image follows the registry mirror" 'image: "mirror.example.com/stateracloud/flavor-opencode:' "${prepull[@]}" \
	--set global.imageRegistry=mirror.example.com --show-only templates/flavor-prepull.yaml
has "flavor pre-pull pause image follows the registry mirror" 'image: "mirror.example.com/pause:3.10"' "${prepull[@]}" \
	--set global.imageRegistry=mirror.example.com --show-only templates/flavor-prepull.yaml
has "flavor pre-pull uses the image pull secrets" '- name: regcred' "${prepull[@]}" --set 'global.imagePullSecrets[0].name=regcred' \
	--show-only templates/flavor-prepull.yaml
lacks "flavor pre-pull sets no runtime class (it is not a sandbox)" 'runtimeClassName' "${prepull[@]}" --show-only templates/flavor-prepull.yaml
lacks "flavor pre-pull is not privileged" 'privileged: true' "${prepull[@]}" --show-only templates/flavor-prepull.yaml
has "flavor pre-pull drops all capabilities" 'drop: \[ALL\]' "${prepull[@]}" --show-only templates/flavor-prepull.yaml
if render --set flavorPrepull.enabled=true >/dev/null 2>&1; then fail "flavorPrepull without a nodeSelector must not render"; fi

