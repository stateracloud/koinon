# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the CA bundle for remote MCP upstreams (gateway.standalone.mcpUpstreamCA).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# Default: no CA Secret is mounted and the reconciler gets no CA file.
lacks "no MCP CA mount by default" 'mcp-ca' --show-only templates/gateway-standalone.yaml
lacks "no MCP CA file by default" 'mcpUpstreamCaFile' --show-only templates/config.yaml

ca=(--set gateway.standalone.mcpUpstreamCA.secretName=mcp-upstream-ca)
has "MCP CA Secret volume" 'secretName: mcp-upstream-ca$' "${ca[@]}" --show-only templates/gateway-standalone.yaml
has "MCP CA mount path" 'mountPath: /secrets/mcp-ca$' "${ca[@]}" --show-only templates/gateway-standalone.yaml
has "reconciler gets the MCP CA file" 'mcpUpstreamCaFile: /secrets/mcp-ca/ca\.crt$' "${ca[@]}" --show-only templates/config.yaml
has "a custom key is used" 'mcpUpstreamCaFile: /secrets/mcp-ca/root\.pem$' "${ca[@]}" --set gateway.standalone.mcpUpstreamCA.key=root.pem --show-only templates/config.yaml

# Kubernetes mode is frozen (ADR 0097 decision 4): the value must not render there.
if render "${ca[@]}" --set gateway.mode=kubernetes >/dev/null 2>&1; then fail "gateway.standalone.mcpUpstreamCA must not render in kubernetes mode"; fi
